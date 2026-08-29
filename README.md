# wbjp — Webull証券 日本株・米国株 自動売買システム

Webull証券の OpenAPI を使って日本株・米国株を売買するシステム。3つのパッケージからなる。

| パッケージ | 役割 | CLI |
|---|---|---|
| `wbcore` | 共通基盤。ブローカー・足データ・指標・認証情報・登録簿の仕組み | — |
| `wbjp` | スイング売買。複数の戦略を差し替え・合成し、差分だけを発注する | `uv run wbjp` |
| `accum` | 積立。ドル平均法＋下落局面での増額。売らない | `uv run accum` |

`wbjp` と `accum` は互いに import しない。どちらも `wbcore` の部品を組み合わせて動く。

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

## 米国株

市場ごとの制約は `wbcore.domain.market_rules` に集約してあり、設定の `universe.market` を切り替えるだけでエンジン本体は同じコードで動く。米国株の設定例は [`config/us/`](config/us/) にある。

```bash
uv run wbjp data sync --config-dir config/us
uv run wbjp backtest --from 2024-01-01 --config-dir config/us
uv run wbjp run --config-dir config/us            # dry-run
```

| | 日本株（`market = "JP"`） | 米国株（`market = "US"`） |
|---|---|---|
| 売買単位 | 100株（単元） | 1株 |
| 呼値 | 価格帯で段階（`jp_rules`） | 0.01ドル固定 |
| 制限値幅 | あり（指値が弾かれる） | なし |
| 逆指値 | API 非対応 → エンジン合成 | **STOP_LOSS を GTC でブローカーに置く** |
| 差金決済の回避 | 当日買った銘柄は当日売らない | 不要 |
| 通貨 | JPY | USD（`risk` の金額もドル建て） |
| 足データ | yfinance | yfinance か Webull 市場データ API |
| 売買手数料 | 約 0.11%（UAT 実測） | 無料（2026-07-27〜）＋ SEC/FINRA 手数料 |

米国株で逆指値をブローカーに置く場合の要点:

- ストップ価格の記録（`stops` テーブル）は引き続きエンジンが持つ。毎サイクル、記録と板上の逆指値を突き合わせて差分だけ置き直す（`wbjp.risk.stops.sync_broker_stops`）。冪等なので再実行しても増えない。
- **逆指値は実効ポジションに数えない。** 数えると「もうすぐ建玉が消える」と誤認して買い直す。`effective_quantity` が除外する。
- 戦略が手仕舞いを決めた銘柄は、売り注文を出す前に逆指値を取り消す。両方が板に乗ると二重売却になる。
- 日足のストップ判定も保険として残す。逆指値が何かの理由で消えていた日でも翌寄付で手仕舞える。
- `execution.stop_mode = "engine"` にすれば米国株でも日足判定だけに戻せる（比較検証用）。

同じ口座に日本株と米国株が混在するため、`WebullBroker` は **設定された市場の通貨の行だけ**を残高・建玉として読む。日本株と米国株を両方回すなら、設定ディレクトリを分けて別プロセスで動かす。

### スクリーニングと順位付け（`trend_pullback`）

米国株の設定は `trend_pullback` 戦略が「スクリーニング＋順位付け」を兼ねる。`config/us/universe.txt` の銘柄（＝allowlist）を毎サイクル全件評価し、条件を満たした銘柄にスコア（`direction` 0.3〜1.0）を付ける。サイジングは direction の高い順に `sizing.max_positions` の枠を埋めるので、**スコアがそのまま採用順位**になる。

```bash
uv run wbjp screen --config-dir config/us                 # 順位表
uv run wbjp screen --config-dir config/us --show-failed   # 落ちた銘柄と理由
```

| 条件 | 意図 |
|---|---|
| 終値 > SMA200 かつ SMA50 > SMA200、SMA50 が上向き | 長期上昇トレンドのみ |
| 60日高値からの下落 ≦ 15% | 押し目であって崩れではない |
| 前日の RSI(3) < 20 ＋ 当日が陽線 | 売られすぎ ＋ 反転確認（落ちるナイフを受けない） |
| ATR/終値 1.5〜5%、20日平均売買代金 ≧ 500万ドル | 動く・滑らない銘柄 |
| SPY > SMA50 | 地合いフィルタ |
| 決算 3 営業日前〜当日は新規建てせず、保有中なら手仕舞い | 日足ではギャップを避けられない（`config/us/earnings.toml` を手動更新） |

スコア = 0.35×押し目の深さ（RSI） + 0.30×SMA20 からの乖離（ATR 単位） + 0.20×トレンドの強さ + 0.15×流動性。内訳は `signals.meta_json` に残る。

手仕舞いは3層: 戦略（RSI(3) ≧ 80 / 含み益で SMA20 回復 / 60日高値到達）、`[stops]`（+1R で建値移動 → ATR トレーリング、5営業日で含み益なし、最大10営業日）、そして初期ストップ（1.5×ATR、米国株は逆指値として板に置く）。

### モメンタム順位戦略（`momentum_rank`、`config/us-momentum/`）

押し目買いとは別の、**損益比型**の戦略。過去 6 ヶ月（直近 1 ヶ月を除く）のリターンをボラティリティで割った順位で上位 `top_n` 銘柄を持ち、月初にだけ入れ替える。数十年・複数市場で生き残っているモメンタム効果に、地合いフィルタ（SPY > SMA200 でなければ全建玉手仕舞い）を重ねてモメンタムクラッシュを避ける。

```bash
uv run wbjp data sync --config-dir config/us-momentum --days 1500   # 12ヶ月の履歴＋ウォームアップが要る
uv run wbjp screen   --config-dir config/us-momentum
uv run wbjp backtest --config-dir config/us-momentum --from 2023-09-01
```

| | 押し目買い（`config/us`） | モメンタム（`config/us-momentum`） |
|---|---|---|
| 狙い | 勝率（小さく多く） | 損益比（大きく少なく） |
| 建て | 条件成立日に毎日 | 月初の営業日だけ |
| 降り | RSI 過熱・SMA20 回復・時間切れ | 終値 < SMA100・順位脱落・地合いオフ |
| 損切り | 1.5×ATR（＋建値移動・利確） | 3×ATR トレーリングのみ |
| 想定勝率 / 損益比 | 60% / 1:1 | 45% / 2.5:1 |

> 押し目買いの検証では、戦略の手仕舞い自体は勝率 90% で機能した一方、タイトな損切りと時間切れが利益を刈っていた（出口の非対称性）。モメンタムはその逆で、出口を広く取って勝ちを伸ばす。

### ロス・キャメロン流モメンタム（`ross_cameron`、`config/us-cameron/`）

Warrior Trading のロス・キャメロンの手法（Gap & Go / マイクロプルバック、9EMA トレーリング、損益比 2:1）を**日足のスイングに翻訳**したもの。本家は分足のデイトレードなので、翻訳の対応表と条件の一覧は [ross_cameron.py](src/wbjp/strategy/samples/ross_cameron.py) の冒頭にまとめてある。

```bash
uv run wbjp data sync --config-dir config/us-cameron --days 400
uv run wbjp screen   --config-dir config/us-cameron
uv run wbjp backtest --config-dir config/us-cameron --from 2019-01-01
```

| 入口 | 条件 |
|---|---|
| Gap & Go（材料日に乗る） | 始値が前日終値比 ≧ 3%、RVOL ≧ 2x、終値がレンジ上位 30% の陽線、直近 20 日高値を上抜け、終値 > EMA9 > EMA20 |
| マイクロプルバック（材料日のあと） | 直近 5 日以内に材料日 → 1〜3 日の押し目（安値が EMA9 の上）→ 前日高値を出来高伴って陽線で抜ける |

スコア = 0.35×RVOL + 0.30×ギャップ + 0.20×引け強度 + 0.15×ブレイク幅（ATR 単位）。出口は戦略側の終値 < EMA9 と、`[stops]` の -5% 初期ストップ／+1R 建値移動／+2R 半分利確／3 営業日で含み益なし。決算は本家にとって「材料」なのでブラックアウトは既定で無効。

> 戦略は名前で呼び分ける。`config/<dir>/strategies.toml` の `name` を変えれば、既存の戦略を上書きせずに別の手法を試せる。使える戦略は `uv run wbjp strategies` で一覧できる。

---

## 積立（`accum`）

スイング売買とは別プロジェクト。「今月いくら**追加**するか」を決めて買い増すだけで、売却も損切りもしない。設定は [`config/accum/accum.toml`](config/accum/accum.toml)、APIキーとデータ置き場は `wbjp` と共有する。

| | スイング売買（`wbjp`） | 積立（`accum`） |
|---|---|---|
| 戦略の出力 | 銘柄ごとに −1.0〜+1.0 の意見 | その日の購入倍率（1.0 以上） |
| 実行 | 合成 → サイジング → 差分発注 | 予算 × 倍率 → 成行買い |
| 設定 | `settings.toml` + `strategies.toml` の `[[strategies]]` | `accum.toml` の `[[tactics]]` / `[[baskets]]` |
| 例 | `trend_pullback`, `ross_cameron`, `momentum_rank` | `bear_stack`, `stack_ladder`, `drawdown_ladder` |

```bash
uv run accum strategies          # 使える戦略
uv run accum list                # 戦略と銘柄の対応
uv run accum sync                # 足を取る（約30年ぶん。増額は暴落局面で効くため長い履歴が要る）
uv run accum plan                # 直近の投下額
uv run accum backtest            # 銘柄ごとの結果（対照群＝同額を均等に投じた場合）
uv run accum compare 1305.T      # 登録済み戦略を1銘柄で横並び
uv run accum run                 # 最新日の投下額を注文にする（dry-run）
uv run accum run --live          # 実発注。本番は WBJP_ENV=prod と --yes も
```

`run` は日足の最終行が入金日（月初）か増額日のときだけ注文を作る。株数は `投下額 ÷ 終値` を単元に切り捨て。ETF は単元が 1 株・10 株のものが多いので `[execution] lot_size_overrides` で指定する（既定の 100 株だと月の予算が届かず丸ごと見送りになる）。注文 ID は日付・銘柄・株数から決定論的に作るため、cron が二重に走っても二重買付にならない。

部品は `wbcore` と共有: 足は `wbcore.data`、発注は `wbcore.broker`、登録簿の仕組みは `wbcore.registry`。積立固有なのは倍率（`accum.tactics`）・計画（`accum.plan`）・検証（`accum.simulate` / `accum.basket`）・注文化（`accum.execute`）で、どの段も単独で差し替えられる。

> **UAT で未確認の点**: 米国株の発注ペイロード（`trade_currency` / `extended_hours_trading` / `support_trading_session` の値）と、Webull 市場データ API の応答形式は SDK と公開ドキュメントから組んであり、実機での疎通確認がまだ。最初は必ず `WBJP_ENV=uat` の dry-run で `wbjp account` と `wbjp run` を通し、ログの発注ペイロードを確認すること。

---

## 取引所（ブローカー）の差し替え

発注先は `wbcore.broker.base.Broker` 抽象クラスの裏に隠れている。売買（`wbjp`）も積立（`accum`）も `wbcore.broker.registry.connect(name, env, market=...)` を通るので、設定の1行で切り替わる。

```toml
[execution]
broker = "webull"   # webull | paper（ネットワークに繋がないシミュレータ）
```

取引所を足す手順:

1. `Broker` を継承し、`name`（設定で使う名前）と `connect()`（認証情報の解決・接続先の選択など、その証券会社固有の準備）を書く
2. `wbcore.broker.registry.BROKERS.register(YourBroker)` する

認証情報は証券会社ごとに名前空間を分ける（`load_credentials(env, namespace="XXX")` → `XXX_PROD_APP_KEY` / キーチェーン `xxx/prod`）。Webull は `WBJP`。CLI には手を入れない。

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

APIキーはリポジトリに置かない。
（SDK はエラー時にリクエストヘッダ全体をログ出力するため、ログには秘匿情報マスクを必ず通す。）

---

## APIキーの置き場所

3つのソースを上から順に見て、**項目ごとに**最初に見つかった値を使う。

| 優先 | ソース | 用途 |
|---|---|---|
| 1 | 環境変数 | systemd の `EnvironmentFile=` / CI（サーバー運用の推奨） |
| 2 | `.env` | キーチェーンの無いホスト。**`chmod 600` 必須** |
| 3 | OS キーチェーン | ローカル開発の推奨 |
| 4 | 公開テスト口座 | UAT のみ。認証情報が無いときの自動フォールバック |

変数名は `WBJP_<ENV>_APP_KEY` / `_APP_SECRET` / `_ACCOUNT_ID`（例: `WBJP_PROD_APP_KEY`）。
どこから読まれているかは `wbjp credentials check --env prod` の「取得元」で確認できる。

### ローカル開発（macOS など）

```bash
uv run wbjp credentials set --env uat   # キーチェーンに保存
```

### Linux サーバー

ヘッドレスな Linux には SecretService も D-Bus も無く keyring が使えないため、
秘密は systemd 経由で環境変数として渡すのが堅い。

```bash
sudo install -d -m 750 -o root -g wbjp /etc/wbjp
sudo install -m 640 -o root -g wbjp /dev/null /etc/wbjp/wbjp.env
sudo vi /etc/wbjp/wbjp.env   # WBJP_PROD_APP_KEY=... 等
```

```ini
# /etc/systemd/system/wbjp.service
[Service]
User=wbjp
EnvironmentFile=/etc/wbjp/wbjp.env
WorkingDirectory=/opt/wbjp
ExecStart=/opt/wbjp/.venv/bin/wbjp run --live
```

リポジトリ内の `.env` は `WBJP_ENV` やログ設定など秘密でない項目に使う。
`.env` に秘密を書く場合は `chmod 600` すること（緩ければ起動時に警告が出る）。

`.env` は `dotenv_values()` で読んでおり、値は `os.environ` に**入れない**。
`load_dotenv()` と違って子プロセスや `/proc/<pid>/environ` から秘密が読めない。

---

## 使い方

```bash
# UAT（実弾なし・公開テスト口座）
WBJP_ENV=uat uv run wbjp account
WBJP_ENV=uat uv run wbjp run --live

# バックテスト
uv run wbjp backtest --from 2023-01-01 --to 2026-06-30
# 約定を Backtrader（Cerebro/Broker）に任せた第2エンジンで突き合わせる。
# 判断ロジックは同一なので、成行なら自前エンジンと約定が一致するはず。
# 差が出たらどちらかの約定モデルにバグがある（指値だけは判定基準が違うので差が出る）
uv run wbjp backtest --from 2023-01-01 --to 2026-06-30 --engine backtrader

# 本番（--live なしは常に dry-run）
WBJP_ENV=prod uv run wbjp run
WBJP_ENV=prod uv run wbjp run --live
```

---

## cron で回す

`wbjp run` は**1サイクルだけ**実行して終了する。定期実行はここに書く cron が担当する。

日足で判断するため、走らせるのは**大引け後に1日1回**。yfinance の当日足が
確定するまで少し待つので、16:00 JST 以降にしておく。

```cron
# 平日 16:30 に日次サイクルを実行
CRON_TZ=Asia/Tokyo
30 16 * * 1-5 cd /opt/wbjp && WBJP_ENV=prod /opt/wbjp/.venv/bin/wbjp run --live --yes >> /var/log/wbjp/run.log 2>&1
```

cron で詰まりやすい点:

- **`--yes` が要る。** 本番の `--live` は確認プロンプトを出すが、cron には
  stdin が無い。`--yes` が無いと理由をログに残して exit 1 で止まる
  （黙って Abort はしない）
- **`cd` が要る。** `config/` `data/` `.env` はカレントディレクトリ基準。
  cron は `$HOME` で起動するので、`cd` しないと設定が見つからない。
  `cd` を使わないなら `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` で `.env` を
  絶対パス指定できる
- **フルパスで呼ぶ。** cron の `PATH` は最小限。`uv run` ではなく
  `.venv/bin/wbjp` を直接叩く
- **祝日は考慮されない。** 東証の休場日にも起動するが、新しい足が増えないため
  基準日が前営業日のままになり、同じ注文IDが再生成されて冪等に弾かれる
- **同じ日の二重実行は安全。** 注文IDは「取引日 × 銘柄 × 売買 × 数量」から
  決定論的に作られるので、cron が二重に走っても、失敗後に手で再実行しても、
  同じ注文が2回出ることはない

止めたいときは `config/settings.toml` の `risk.kill_switch = true`。
cron を消さなくても次のサイクルから発注しなくなる。

---

## 構成

```
src/wbcore/              共通基盤（wbjp / accum のどちらからも使う。逆向きの依存は無い）
├── credentials.py       認証情報の解決（キーチェーン / 環境変数 / .env。秘密はここを通す）
├── settings.py          環境変数由来の設定（WBJP_ENV など）と実発注の可否判定
├── registry.py          名前 → クラス の登録簿（両プロジェクトの戦略登録に使う）
├── logging.py           構造化ログ + 秘匿情報マスク
├── domain/
│   ├── models.py        Bar / Signal / Order / Position（すべて Decimal）、決定論的な注文ID
│   ├── market_rules.py  JP / US の取引ルールの抽象化
│   └── jp_rules.py      呼値・値幅制限・単元株・取引時間・差金決済
├── data/                MarketDataProvider → yfinance / Webull / CSV / Parquet+DuckDB / EDGAR 13F
├── indicators/ohlcv.py  polars 式で SMA/EMA/RSI/ATR/MACD/BB/ADX/Donchian
└── broker/              Broker ABC → Webull / Paper / レート制限 / 接続の組み立て（factory）

src/wbjp/                スイング売買
├── config.py            settings.toml / strategies.toml（ユニバース・リスク・出口・レジーム）
├── strategy/            Strategy ABC / 合成器4種 / サンプル戦略7本
├── portfolio/sizer.py   equal_weight / fixed_notional / atr_risk
├── risk/                リスク上限 / ストップの合成
├── engine/              リコンサイル / バックテスト / 日次ランナー
├── db/                  SQLite の判断ジャーナル
└── cli.py               `wbjp`

src/accum/               積立
├── config.py            accum.toml（戦略と銘柄の対応・予算・発注）
├── tactics.py           Tactic ABC / constant / bear_stack / stack_ladder / drawdown_ladder
├── stack.py, window.py  移動平均の配列判定 / 発注時間帯
├── plan.py              日足と戦略 → 日ごとの投下額
├── simulate.py, basket.py  検証（対照群との比較 / 複数銘柄への配分・13F）
├── execute.py           投下額 → 成行の買い注文（wbcore.broker へ渡す）
└── cli.py               `accum`
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
| `engine/bt_engine.py` | Backtrader の DAY 注文は日足だと翌バーの前に失効して一度も約定しないため、GTC で出して橋渡し側が翌日に取り消す。指値はバー内の高安で約定判定される（PaperBroker は寄付だけ）ので、突き合わせは成行で行う |

## 開発

```bash
uv run pytest              # ネットワークを使うものは既定でスキップ
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

