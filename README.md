# wbjp — Webull証券 日本株 自動売買システム

Webull証券の OpenAPI を使って日本株を売買するシステム。複数の売買戦略を差し替え・合成して実行できる。

> **⚠️ 免責**
> 本ソフトウェアは投資助言ではありません。自動売買は資産を失う可能性があります。
> 必ず UAT（テスト）環境で十分に検証し、自己責任で利用してください。

---

## 設計の前提（実機調査で確定）

Webull JP OpenAPI には、設計を左右する2つの制約がある。

### 1. 日本株の株価データは Webull API から取得できない

Webull の市場データAPI（`bars` / `snapshot`）は `US_STOCK` / `US_ETF` にしか対応していない。
日本株は **発注はできるが、足データは取れない**。

→ **価格データ層とブローカー層を完全に分離**し、価格は yfinance（`7203.T` 形式）から取得する。

### 2. 日本株は成行・指値のみ。逆指値は使えない

| 注文種別 | 米国株 | 日本株 |
|---|:---:|:---:|
| MARKET（成行） | ✓ | ✓ |
| LIMIT（指値） | ✓ | ✓ |
| STOP_LOSS（逆指値） | ✓ | **✗** |
| STOP_LOSS_LIMIT | ✓ | **✗** |

→ **損切りはエンジン側で合成する**（`wbjp.risk.stops`）。
ストップ価格をローカルに保持し、日足更新時に評価して成行/指値の決済注文を出す。

> **重要な限界**: 日足運用のため、場中の急落にはその日のうちに反応できない。
> 想定より大きな損失が出る可能性がある。この制約を理解した上で使うこと。

---

## アーキテクチャ

戦略は「注文」ではなく「シグナル」を出す。注文への変換はエンジンが一手に引き受ける。

```
Strategy.on_bars(ctx) -> list[Signal]     戦略はI/Oを一切知らない
        ↓
SignalCombiner        複数戦略のシグナルを1本に合成
        ↓
PositionSizer         合成シグナル → 目標株数（単元株に丸め）
        ↓
Reconciler            目標 vs 実建玉＋未約定 の「差分だけ」を注文化
        ↓
RiskManager           上限チェック・拒否
        ↓
Broker                WebullBroker / PaperBroker
```

この構造が効くところ:

- **冪等性** — 「差分だけ発注」なので、クラッシュ後に再実行しても二重発注しない
- **テスト容易性** — 戦略は純粋な関数に近く、モック不要で単体テストできる
- **同じコードがバックテストでも本番でも動く** — `Broker` を差し替えるだけ

---

## 技術スタック

| 領域 | 選定 | 理由 |
|---|---|---|
| 言語 | Python 3.14 | SDK が `<3.15` のため上限。全依存のcp314ホイールを確認済み |
| パッケージ管理 | uv | ランタイムごと管理。ロックファイルで再現性 |
| データフレーム | polars | インジケーターは polars 式で自前実装 |
| 価格データ | yfinance | 日足スイングには十分。取得済みは必ずローカルキャッシュ |
| 状態の保存 | SQLite | 注文・シグナル・実行履歴。ACID と冪等性の担保 |
| 時系列の保存 | Parquet + DuckDB | 足データの高速な集計・分析 |
| 発注 | webull-openapi-python-sdk | 公式SDK |

---

## セットアップ

```bash
uv sync
```

`uv` が Python 3.14 を自動で取得する。システムのPythonには一切触らない。

---

## 安全装置

実発注には**2つの条件が同時に**必要。どちらか欠けたら dry-run になる。

1. `WBJP_ENV=prod`
2. CLI に `--live` フラグ

さらに `config/settings.toml` の `risk.kill_switch = true` で全発注を即停止できる。

APIキーは macOS Keychain に保管し、リポジトリにも `.env` にも秘密は置かない。
（SDK はエラー時にリクエストヘッダ全体をログ出力するため、ログには秘匿情報マスクを必ず通す。）

---

## 使い方

```bash
# UAT（実弾なし・公開テスト口座）
WBJP_ENV=uat uv run wbjp account
WBJP_ENV=uat uv run wbjp run --live

# バックテスト
uv run wbjp backtest --from 2023-01-01 --to 2026-06-30

# 本番（--live なしは常に dry-run）
WBJP_ENV=prod uv run wbjp run
WBJP_ENV=prod uv run wbjp run --live
```

---

## 構成

```
src/wbjp/
├── config.py            設定 + キーチェーン（秘密はここを通す）
├── logging.py           構造化ログ + 秘匿情報マスク
├── domain/
│   ├── models.py        Bar / Signal / Order / Position（すべて Decimal）
│   └── jp_rules.py      呼値・値幅制限・単元株・取引時間・差金決済
├── data/                MarketDataProvider → yfinance / CSV / Parquet+DuckDB
├── indicators/ohlcv.py  polars 式で SMA/EMA/RSI/ATR/MACD/BB/ADX/Donchian
├── strategy/            Strategy ABC / 合成器4種 / サンプル戦略3本
├── portfolio/sizer.py   equal_weight / fixed_notional / atr_risk
├── risk/                リスク上限 / ストップの合成
├── broker/              Broker ABC → Webull / Paper / レート制限
├── engine/              リコンサイル / バックテスト / 日次ランナー
├── db/                  SQLite の判断ジャーナル
└── cli.py               typer
```

### 実装上の注意点（実機で確認した挙動）

| 箇所 | 内容 |
|---|---|
| `logging.py` | SDK は起動時に**自前のログハンドラとログファイル**を作り、マスクを迂回する。`harden_third_party_logging()` と `_suppress_sdk_own_logging()` で塞いでいる |
| `broker/webull.py` | 注文照会は**コンボ構造**（実データは入れ子の `orders` 配列）。外側だけ読むと空の注文に見える |
| `broker/webull.py` | `TradeClient()` はコンストラクタでネットワークを叩くため遅延初期化 |
| `jp_rules.py` | 呼値は「以下」区分、値幅制限は「未満」区分。引き方が違う |
| `indicators/ohlcv.py` | `diff()` の先頭 null は `pl.when` で明示的に通す。0 に化けると Wilder 平滑化の種が汚れて TA-Lib と値がずれる |
| `engine/backtest.py` | 指標は全期間で一度だけ計算する（指標が因果的なので等価）。毎日再計算すると約50倍遅い |
| `broker/paper.py` | 注文の失効は**約定処理のあと**。先に失効させると注文が一度も約定しない |

## 開発

```bash
uv run pytest              # 375件（ネットワークを使うものは既定でスキップ）
uv run pytest -m network   # yfinance 実接続
uv run ruff check .
uv run mypy                # strict
```

## 次にやること

1. `config/settings.toml` の `universe.symbols` を実際に売買したい銘柄に変える
   （TOPIX500 構成銘柄は `topix500_symbols` にも入れる。呼値が変わる）
2. `risk.max_order_value_jpy` を自分の資金規模に合わせる
   （既定の50万円は保守的。UAT の9,800万円口座では全注文が上限に当たる）
3. UAT で `wbjp run --live` を数日回し、`wbjp explain <run_id>` で判断を目視検証
4. 本番のAPIキーを申請 → `wbjp credentials set --env prod`
5. `WBJP_ENV=prod wbjp run`（dry-run）を数日回してから `--live` に進む

