# ログの形式（機械が読む用）

後から AI に読ませて改善に使う前提で、ログは **JSON Lines**（1 行 1 レコード、UTF-8）で
ファイルに残す。端末に出る整形済みの表示とは別の経路で、常に書かれる。

| 何 | どこ |
|---|---|
| 置き場 | `WBJP_LOG_DIR`（既定 `WBJP_STATE_DIR/logs`＝`state/logs`）。**ファイルに残すログはこの 1 箇所だけ。** cron で stderr を残すときもここへ。本番では絶対パスで指定する |
| ファイル | `<WBJP_LOG_DIR>/<app>-<env>.jsonl`。`app` は `wbjp` / `accum` / `jquants` / `daytrade`、`env` は `uat` / `prod` |
| ローテーション | 日次（UTC の 0 時）、90 日保持。ローテーション後は `…jsonl.YYYY-MM-DD` |
| 文字 | UTF-8、`ensure_ascii=False`（日本語はそのまま） |
| 鍵の並び | 辞書順（`sort_keys`）。差分を取りやすくするため |
| 秘匿情報 | API キー・シークレット・口座 ID はファイルに書く前に `***` に置き換わる |

## 全レコードに付く項目

| 項目 | 意味 | 例 |
|---|---|---|
| `schema` | この形式の版。項目を変えたら上げる | `wbjp.log.v1` |
| `ts_utc` | 記録時刻（**常に UTC**、オフセット付き ISO 8601）。並べ替えと突き合わせの鍵 | `2026-08-29T07:32:39.123456+00:00` |
| `timestamp` | 同じ時刻を表示用の時間帯で（`WBJP_TIMEZONE`）。人が読む用 | `2026-08-29T16:32:39.123456+09:00` |
| `level` | `debug` / `info` / `warning` / `error` | |
| `logger` | 出したモジュール | `accum.cli` |
| `event` | 出来事の説明（日本語） | `積立の判断` |
| `run_id` | 1 回の CLI 実行の識別子。同じ実行のログを 1 本の線として読む | `3f9a1c…` |
| `app` | `wbjp`（スイング売買）/ `accum`（積立） | |
| `env` | `uat` / `prod` | |
| `command` | 実行したサブコマンド | `run` / `data sync` |
| `code` | 出来事の**安定した識別子**（付いているものだけ）。集計や分類はこれで行う | `accum.decision` |

`event` は人向けの文で、文言が変わることがある。分類には `code` を使うこと。

## `code` の一覧

### J-Quants の蓄積（`jquants`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `jquants.ingest` | 端点 1 つ・対象 1 つを取り込んだ | `endpoint`, `target`（日付 / `bulk:<file>`）, `source`（`api` / `bulk`）, `rows`, `changed`（鍵で上書きして実際に変わった行数） |
| `jquants.gap` | `check` が営業日の欠けを見つけた | `missing` |
| `jquants.no_calendar` | 取引カレンダーが無く平日で代用した | — |

### 積立（`accum`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `accum.fill` | 前回までに送った注文の約定状況が変わった | `symbol`, `client_order_id`, `before`, `after`, `filled`, `quantity`, `lost_ratio`（未約定のまま終わった割合。0 なら全部約定） |
| `accum.stale` | 足が古くて判定を見送った | `symbols`（銘柄 → 最終足の日付） |
| `accum.lot_size` | 売買単位が設定と銘柄情報で食い違った（API を採用）、または照会できず設定値で進んだ | `symbol`, `configured`, `api` / `market`, `error` |
| `accum.decision` | 今日出すべき投下を決めた | `symbol`, `market`, `month`（どの月の積立か）, `judged_on`（判断日）, `target`（今月の目標）, `placed`（発注済み）, `due`（差額＝今日出す額）, `multiplier`, `price`, `tactic`, `signal`（判定用の銘柄。無ければ null） |
| `accum.order` | 投下を注文にした結果（1 件ごと） | `symbol`, `client_order_id`, `quantity`, `price`, `amount`, `live`（実発注か）, `outcome`（`発注` / `dry-run` / `見送り …` / `失敗 …` / `発注済み（冪等）`）, `note`（見送り・失敗の理由） |
| `accum.unconfirmed` | 発注を送ったが応答が返らず、届いたか分からない。台帳には `PENDING` のまま残し再送しない（`accum orders --check` で確かめる） | `symbol`, `client_order_id`, `error` |
| `accum.run` | 実行の終了 | `live`, `reason`（dry-run の理由）, `orders`, `failures` |
| `accum.crash` | 実行が例外で異常終了した（通知も送る）。exit 1 | `error`, `exception`（トレースバック本文。`log.exception` / `exc_info=True` のログはすべてこの項目を持つ） |

金額・数量・価格は **文字列**（`"25000"`）で入っている。JSON の数値にすると Decimal の精度が失われるため。

### デイトレ（`daytrade`）

不具合の再現に要る 4 つ——**そのとき有効だった設定**（`daytrade.config`）、**入力**（`daytrade.plan` / `daytrade.quotes` /
`daytrade.regime`）、**判断**（`daytrade.ranking` / `daytrade.pick` / `daytrade.skip`）、**結果**（`daytrade.order` / `daytrade.fill` /
`daytrade.run` / `daytrade.crash`）——を毎回の実行に残す。1 回の実行は `run_id` で束ねる。

| `code` | いつ | 主な項目 |
|---|---|---|
| `daytrade.config` | 実行の冒頭（plan / open / close） | `phase`, `day`, `live`, `enabled`, `max_capital`, `positions`（N）, `budget_per_order`, `segments`, `min_turnover`, `max_gap`, `skip_months`, `iv_gate`, `drift_gate`, `equity_curve_days`, `us_skip`, `quote_source`, `entry_window` / `exit_window`, `max_quote_age`, `kill_switch`, `watch_only`, `state_dir`, `data_dir` |
| `daytrade.plan` | 前夜に候補を作った | `day`, `prev_day`, `candidates`（全銘柄）, `eligible`（対象）, `positions`, `budget`, `iv_prev`, `path` |
| `daytrade.quotes` | 気配を取った／使えない気配を除外した | `source`, `requested`, `received`, `missing`, `missing_sample`（取れなかった銘柄、最大 20） / `stale`, `stale_sample`, `delayed`, `delayed_sample`, `max_age_sec`, `oldest` |
| `daytrade.regime` | 危険信号を評価した（毎朝） | `day`, `trade`, `reasons`, `month`, `iv_prev`, `drift_bp`, `market_gap_bp`, `recent_pnl`, `us_ret_bp`, `vix` |
| `daytrade.ranking` | ギャップ順の順位表（N と次点 5 件） | `day`, `n`, `budget`, `scale`, `weighting`, `quotes`, `rows`（`rank`, `symbol`, `gap`, `price`, `vol`, `quantity`, `picked`） |
| `daytrade.pick` | 買う銘柄を決めた（1 件ごと） | `day`, `symbol`, `code_`（J-Quants の 5 桁）, `rank`, `gap`, `prev_close`, `price`, `quantity`, `amount` |
| `daytrade.order` | 注文にした結果（1 件ごと。買いも売りも） | `day`, `symbol`, `side`, `client_order_id`, `quantity`, `price`, `amount`, `live`, `outcome`（`発注` / `dry-run` / `見送り …` / `失敗 …`） |
| `daytrade.carry` | verify が売れ残り（持ち越し）を見つけた（通知も送る） | `day`, `positions`（銘柄と株数） |
| `daytrade.fill` | close / verify が注文をブローカーに照会した | `symbol`, `client_order_id`, `broker_order_id`, `before` / `after`（状態）, `quantity`, `filled`, `avg_fill_price`。照会できなければ warning で `after` が null |
| `daytrade.reconcile` | close / verify が台帳と食い違う建玉をブローカーに見つけた（通知も送る）／建玉を照会できなかった | `held`, `symbol` / `error` |
| `daytrade.skip` | 何もしなかった | `reason`（`disabled` / `holiday` / `window` / `regime` / `already` / `no_quotes` / `no_picks` / `no_capital` / `no_buys` / `nothing_to_sell`）と付随項目 |
| `daytrade.pnl_incomplete` | 資産曲線の評価で、売りの約定単価が確定していない日を除いた | `days` |
| `daytrade.iv_missing` / `daytrade.us_missing` | 前日の IV／前夜の米国市場を取れず、そのゲート無しで進んだ | `prev_day` / `error` |
| `daytrade.run` | 実行の終了 | `phase`（`open` / `close`）, `live`, `reason`, `n`, `budget`, `picks` / `sells`, `failures` |
| `daytrade.crash` | 実行が例外で異常終了した（通知も送る）。exit 1 | `error`, `exception`（トレースバック） |

ブローカーとのやり取り（送ったペイロード・応答）は、各ブローカー実装が `event` で残す（`発注します` など。`code` 無し）。
気配が取れなかった銘柄は `daytrade.quotes` の `missing_sample` に残る。

### スイング売買（`wbjp`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `wbjp.signals` | 戦略が意見を出した | `strategy`, `signals`（件数） |

`code` の無いログ（サイクル開始・発注・リスク判定の結果など）は `event` と付随項目で読む。
順次 `code` を付けていく。

## 読み方の例

1 回の実行を追う:

```bash
jq -c 'select(.run_id == "3f9a1c…")' state/logs/accum-prod.jsonl
```

積立の判断だけを表にする:

```bash
jq -r 'select(.code == "accum.decision") | [.ts_utc, .symbol, .month, .target, .placed, .due, .multiplier] | @tsv' state/logs/accum-prod.jsonl
```

未約定のまま終わった注文:

```bash
jq -c 'select(.code == "accum.fill" and (.lost_ratio | tonumber) > 0)' state/logs/accum-prod.jsonl
```

AI に渡すときは、対象の期間の行をそのまま渡せばよい。1 行が 1 レコードで自己記述的
（鍵の名前だけで意味が分かる）になっている。
