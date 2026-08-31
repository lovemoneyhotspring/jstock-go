# デイトレ（`daytrade`）— 寄付で買い、大引で売る

日本株のギャップ逆張り `jp_gap_fade` を回す CLI。根拠と数字は
[research/2026-08-jp-daytrade-selection.md](research/2026-08-jp-daytrade-selection.md)。
設定は `config/daytrade/daytrade.toml`、実装は `src/daytrade/`。

## 規則（1 行で）

前夜に「プライム・売買代金 20 日中央値 1 億円以上・時価総額下位 1/3 除外・前日引け後の決算なし・
当日決算予定なし・日々公表信用残の対象でない」銘柄を母集団にし、9:00 の気配で
**ギャップ（寄付 ÷ 前日終値 − 1）が最も小さい N 銘柄**を成行で買い、15:26 以降の成行売りで
クロージング・オークションの引け値で手仕舞う。持ち越さない。

N は資金から決める: `N = round(max_capital ÷ order_budget)`（200 万円 ÷ 67 万円 → 3）。
1 注文は `max_capital ÷ N`。Webull の手数料は 20〜100 万円が一律 275 円なので、1 注文を小さくすると
bp が跳ね上がる（この規則の理由）。

## 1 日の流れ（JST）

| 時刻 | コマンド | 何をするか | 入力 |
|---|---|---|---|
| 20:30（前夜） | `daytrade plan` | 翌営業日の母集団を `state/daytrade/plan-<日付>.parquet` に保存 | J-Quants アーカイブ（`jquants sync` 済みの前日足・銘柄一覧・決算・日々公表） |
| 09:00〜09:15 | `daytrade open --live --yes` | 候補の気配を取り、ギャップ下位 N 銘柄を成行買い。台帳に記録 | plan + 気配（`execution.quote_source`） |
| 15:26〜15:30 | `daytrade close --live --yes` | 台帳の当日買いをブローカーに照会し、約定数量を成行売り | 台帳 + ブローカー |
| 随時 | `daytrade status` | 候補と当日の注文 | |

すべて既定は dry-run。`--live` が無ければ判断と記録だけで注文は出さない。本番口座では
`WBJP_ENV=prod` と `--live`、cron では `--yes` も要る（`accum` と同じ規律）。

- `open` は台帳に当日の買い（dry-run 以外）があれば何もしない（冪等）。注文 ID は日付・銘柄・数量から決まる
- `close` は台帳の買いだけを売る。**ブローカーの建玉を無条件に売らない**（積立の保有を守る）
- `close` の失敗は持ち越しになるので必ず通知する（`WBJP_ALERT_WEBHOOK_URL`）
- IV ゲート（`regime.iv_gate`）は前日の日経 225 オプション `BaseVol` 中央値。オプションの足は 27:00 頃の更新なので
  20:30 の plan には無く、`open` が朝のアーカイブから取り直す。無ければゲート無しで進む（警告ログ）

## 気配の取得元（未解決の前提）

**Webull OpenAPI の市場データは公式ドキュメント上 US 株のみ**（README「設計の前提」、
https://developer.webull.co.jp/apis/docs/market-data-api/overview.md）。日本株のスナップショットが
返るかは実機でしか分からない。取得元は差し替え可能:

| `quote_source` | 中身 | 用途 |
|---|---|---|
| `webull` | 市場データ API のスナップショット（`category=JP_STOCK` を試す） | 本命。`daytrade quotes 7203 9984` で疎通確認 |
| `yfinance` | Yahoo Finance。**東証は 20 分遅れ** | 検証・dry-run のみ。`open` は `--allow-delayed` が無いと使わない |
| `csv` | `symbol,price[,at]` のファイル | 別経路で取った気配を流す |

`webull` が日本株を返さなければ、この戦略は**寄付の判断ができない**。代替は他社のリアルタイム
気配 API（立花証券 e 支店、kabu STATION など）を `QuoteSource` として足すこと。

## 設定（`daytrade.toml`）

| 節 | 項目 | 意味 |
|---|---|---|
| `[capital]` | `max_capital` | 1 日に使う資金の上限（円） |
| | `order_budget` | 1 注文の目安。N の決定に使う（既定 67 万円） |
| | `max_positions` | N の上限（既定 10。研究では N10 超で Sharpe が落ちる） |
| `[universe]` | `segments` | prime / standard / growth。再編前の一部・二部・マザーズも同じ呼び方 |
| | `min_turnover` / `turnover_days` | 売買代金の中央値の下限と日数。上げるほど成績は落ちる |
| | `exclude_cap_terciles` | 時価総額の下位からいくつ外すか（1 が最良） |
| | `exclude_earnings_prev` / `exclude_earnings_today` / `exclude_margin_alert` | 除外条件 |
| `[signal]` | `max_gap` / `min_gap` | ギャップの範囲。既定は「負なら全部」 |
| `[regime]` | `iv_gate` | 0 で常時。18 で高ボラ局面だけ（DD 半分、稼働 5 割） |
| `[execution]` | `quote_source` / `quote_file` | 気配の取得元 |
| | `entry_window` / `exit_window` | 発注してよい時間帯（JST）。外なら何もしない |
| | `max_quote_age` | 気配がこれより古ければ使わない（秒） |
| | `kill_switch` | true で発注を止める |

## バックテスト

```bash
uv run daytrade backtest --since 2017-01-01          # 設定どおり（資金固定・100 株単位・段階手数料）
uv run daytrade backtest --since 2022-01-01 --trades  # 直近と個別の取引
```

前夜の `plan` と同じ式（`daytrade.universe.eligible_expr`）と 9:00 と同じ順位付け
（`daytrade.select.gap_rank_expr`）をアーカイブのパネルに当てる。検証と実運用で条件が
ずれないための構造。

## cron

`docs/DEPLOY.md` の cron の節を参照。`jquants sync` が前日足を取り込んだ後（20:30）に `plan`、
9:00 と 15:26 に `open` / `close`。`flock` で重複起動を防ぐ。

## ログ

`state/logs/daytrade-<env>.jsonl`。`code` は `docs/LOGGING.md` の「デイトレ」の節。
