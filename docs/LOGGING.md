# ログの形式（機械が読む用）

後から AI に読ませて改善に使う前提で、ログは **JSON Lines**（1 行 1 レコード、UTF-8）で
ファイルに残す。端末に出る整形済みの表示とは別の経路で、常に書かれる。

**読み手は AI であって人ではない**。そのため次の 3 段で「読む量」を削ってある。
上から順に見て、必要になったときだけ下に降りる:

| 段 | 何 | どこ |
|---|---|---|
| 1 | **日次ダイジェスト**——実行 1 回が 1 行。異常の有無だけ分かる | `state/digest/<env>-<日付>.jsonl` |
| 2 | **判断・実行品質の履歴**——追記専用の Parquet。列で絞って読める | `state/<app>/history/<種類>/` |
| 3 | この文書の JSONL——再現に要る全部 | `state/logs/<app>-<env>.jsonl` |

これとは別に、**Discord に投稿したものの控え**が `state/notify/<日付>.jsonl` に残る
（1 投稿 1 行、**45 日**）。送れなかったものも `ok: false` と理由付きで残るので、
「昨日の異常通知は何だった？」「レポートは届いたか」に Discord を開かずに答えられる。
項目は `at` / `kind`（`alert` / `report`）/ `title` / `body` / `channel_id` /
`thread_id` / `ok` / `error`。レポートの本文は `state/reports/`（日次 45 日・
週次 180 日・月次は消さない）。

**どの置き場も日付でファイルが分かれている**ので、期間を指定すればその範囲の
ファイルだけを開く。全部読まずに済む構造が、記録が増えても壊れないための前提:

| 置き場 | 1 ファイルの単位 | 期間で絞る方法 |
|---|---|---|
| `state/digest/<env>-<日付>.jsonl` | 1 日 | ファイル名（`prod-2026-09-{01..30}.jsonl`） |
| `state/notify/<日付>.jsonl` | 1 日 | ファイル名。Go からは `notify.ReadArchive(from, to)` |
| `state/logs/<app>-<env>.jsonl.<日付>` | 1 日（退避後） | ファイル名。当日分だけ接尾辞なし |
| `state/<app>/history/<種類>/*.parquet` | 1 実行（名前の先頭 10 文字が判定日） | `history.Store.Files(kind, Range)` が**開く前に**名前で選ぶ。SQL なら `WHERE day BETWEEN …` |
| `state/reports/*.md` | 1 レポート | ファイル名 |

```console
# 期間を絞る（必要な日のファイルしか開かない）
jq -c 'select(.anomalies)' state/digest/prod-2026-09-{01..07}.jsonl
bin/daytrade review --from 2026-09-01 --to 2026-09-07 --json
```

```console
# 昨日の異常通知
jq 'select(.kind == "alert")' state/notify/2026-09-04.jsonl

# 届かなかったものだけ
jq 'select(.ok == false)' state/notify/*.jsonl
```

```console
# まず今日の全実行（数十 KB）。異常のあった実行だけ run_id を拾う
jq 'select(.anomalies)' state/digest/prod-2026-09-03.jsonl

# その run_id だけを JSONL から引く。定型行（routine）は読み飛ばす
jq 'select(.run_id == "abc123" and .routine != true)' state/logs/daytrade-prod.jsonl
```

| 何 | どこ |
|---|---|
| 置き場 | `WBJP_LOG_DIR`（既定 `WBJP_STATE_DIR/logs`＝`state/logs`）。**ファイルに残すログはこの 1 箇所だけ。** cron で stderr を残すときもここへ。本番では絶対パスで指定する |
| ファイル | `<WBJP_LOG_DIR>/<app>-<env>.jsonl`。`app` は `wbjp` / `accum` / `jquants` / `daytrade`、`env` は `uat` / `prod` |
| ローテーション | 日次（UTC の 0 時）、90 日保持。ローテーション後は `…jsonl.YYYY-MM-DD`（日付は**中身が書かれた日**）。**日付ごとにファイルが分かれるので、他の日を読まずに済む**。退避はプロセスの起動時に行う（cron のプロセスはどれも短命なので、その日の最初の 1 本が退避する） |
| 文字 | UTF-8、`ensure_ascii=False`（日本語はそのまま） |
| 鍵の並び | 辞書順（`sort_keys`）。差分を取りやすくするため |
| 秘匿情報 | API キー・シークレット・口座 ID はファイルに書く前に `***` に置き換わる |

## 全レコードに付く項目

| 項目 | 意味 | 例 |
|---|---|---|
| `schema` | この形式の版。項目を変えたら上げる | `wbjp.log.v1` |
| `ts_utc` | 記録時刻（**常に UTC**、オフセット付き ISO 8601）。並べ替えと突き合わせの鍵 | `2026-08-29T07:32:39.123456+00:00` |
| `routine` | 「動いただけ」の定型行に `true`。**付いていない行だけ読めばよい**（`false` は書かない） | `true` |
| `level` | `debug` / `info` / `warning` / `error` | |
| `logger` | 出したモジュール | `accum.cli` |
| `event` | 出来事の説明（日本語） | `積立の判断` |
| `run_id` | 1 回の CLI 実行の識別子。同じ実行のログを 1 本の線として読む | `3f9a1c…` |
| `app` | `wbjp`（スイング売買）/ `accum`（積立） | |
| `env` | `uat` / `prod` | |
| `command` | 実行したサブコマンド | `run` / `data sync` |
| `code` | 出来事の**安定した識別子**（付いているものだけ）。集計や分類はこれで行う | `accum.decision` |
| `verify` | **実機検証で手で走らせた実行**（`--broker-verify`）。付いていない行は通常の運用 | `true` |

`event` は人向けの文で、文言が変わることがある。分類には `code` を使うこと。

### `verify` —— 手で検証したのか、異常が起きたのか

発注経路の実機検証（[BROKER_VERIFY.md](BROKER_VERIFY.md)）は**本番口座（`env=prod`）で
行うことがある**。`env` は口座の選択でしかないので、「時間外に建てた」「持ち越しが出た」が
検証の手順どおりなのか本当の異常なのかを `env` では切り分けられない。

そこで実行そのものに印を持たせる。`--broker-verify` を付けて走らせると、
**その実行のログの全行**に `verify: true` が付き、ダイジェストにも `verify: true` が残る。
通常の実行では項目ごと出ない（既存の形は変わらない）。

```console
# 検証を除いた本当の異常だけを見る
jq -c 'select(.level == "error" and (.verify | not))' state/logs/daytrade-prod.jsonl
# 検証で走らせた実行だけを追う
jq -c 'select(.verify)' state/logs/daytrade-prod.jsonl
jq -c 'select(.verify)' state/digest/prod-2026-09-06.jsonl
```

台帳にも同じ印が付く（`orders.verify`）。検証で建てた玉は**本物**なので `close` / `verify` は
同じように扱うが、成績の集計（`evaluate` / `review` / `trades` / レポート）と
**資産曲線のゲート**（直近 20 日の実現損益）からは外れる——1 単元の検証取引で
翌日の資金が半分に縮まないようにするため。

`timestamp`（表示の時間帯）は**ファイルには残さない**。`ts_utc` と同じ時刻の
二重持ちで、1 行あたり約 55 バイトを占めていた。端末の表示には従来どおり出る。

## 定型行（`routine`）

「いつも通り動いた」だけの行には `routine: true` が付く。付け方は 2 通りで、
呼び出し側の明示が優先する:

1. `log.info("足を更新しました", ..., routine=True)` — `code` の無い行はこちら
2. `code` が `wbcore.logging.ROUTINE_CODES` にある

**判断・発注・異常は絶対に定型にしない。** 同期して変化が無かった、時間帯の外で
何もしなかった——といった「読み飛ばしてよい」行だけに付ける。

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
| `accum.unconfirmed` | 発注を送ったが応答が返らず、届いたか分からない。台帳には `PENDING` のまま残し、次の `run` の冒頭で当日の注文一覧と突き合わせて自動で判定する | `symbol`, `client_order_id`, `error` |
| `accum.pending_resolved` | 送信結果不明の注文を当日の注文一覧で判定した（1 件ごと）。`attributed`（届いていた→注文番号を帰属）/ `not_sent`（届いていない→`UNSENT`、次の差額で埋め直す）/ `too_recent`（送った直後。次の run で） | 本文に銘柄・数量・判定 |
| `accum.pending_ambiguous` | ダイジェストの異常。同じ銘柄で細部の違う未帰属の注文があり自動で決められない。`PENDING` のまま次の run で再判定。続くなら口座の注文一覧を見る | `detail` |
| `accum.run` | 実行の終了 | `live`, `reason`（dry-run の理由）, `orders`, `failures` |
| `accum.crash` | 実行が例外で異常終了した（通知も送る）。exit 1 | `error`, `exception`（トレースバック本文。`log.exception` / `exc_info=True` のログはすべてこの項目を持つ） |

金額・数量・価格は **文字列**（`"25000"`）で入っている。JSON の数値にすると Decimal の精度が失われるため。

### デイトレ（`daytrade`）

不具合の再現に要る 4 つ——**そのとき有効だった設定**（`daytrade.config`）、**入力**（`daytrade.plan` / `daytrade.quotes` /
`daytrade.regime`）、**判断**（`daytrade.ranking` / `daytrade.pick` / `daytrade.skip`）、**結果**（`daytrade.order` / `daytrade.fill` /
`daytrade.run` / `daytrade.crash`）——を毎回の実行に残す。1 回の実行は `run_id` で束ねる。

ログは再現用で、順位表は上位 N+5 件、保持は 90 日。振り返り・検証のための**全行**（候補・気配・順位表・実行の要約）は
`state/daytrade/history/` に追記専用の Parquet で残し、同じ `run_id` で突き合わせられる（`docs/DAYTRADE.md`「履歴」）。
`wbjp screen` も同様に `state/wbjp/history/screen/` に残す（`wbjp.screen` の行に `path`）。

| `code` | いつ | 主な項目 |
|---|---|---|
| `daytrade.config` | 実行の冒頭（plan / open / close） | `phase`, `day`, `live`, `enabled`, `max_capital`, `positions`（N）, `budget_per_order`, `segments`, `min_turnover`, `max_gap`, `skip_months`, `iv_gate`, `drift_gate`, `equity_curve_days`, `us_skip`, `quote_source`, `entry_window` / `exit_window`, `max_quote_age`, `kill_switch`, `watch_only`, `state_dir`, `data_dir`, `deadline`（この実行の締め切り、JST）, `max_run_seconds` |
| `daytrade.resume` | 同じ日の前の回が建てた建玉があり、残りの枚数だけ建て直す | `long`, `short`（建てた数）, `remaining_long`, `remaining_short`, `symbols`（建てた銘柄。候補から外す） |
| `daytrade.plan` | 前夜に候補を作った | `day`, `prev_day`, `candidates`（全銘柄）, `eligible`（対象）, `positions`, `budget`, `iv_prev`, `path` |
| `daytrade.quotes` | 気配を取った／使えない気配を除外した | `source`, `requested`, `received`, `missing`, `missing_sample`（取れなかった銘柄、最大 20）, `elapsed_ms` / `stale`, `stale_sample`（`銘柄@時刻 年齢秒`）, `delayed`, `delayed_sample`, `future`（時刻が 1 分以上先の気配の数。寄り前に前日の時刻が返る罠の観測用）, `future_sample`, `max_age_sec` |
| `daytrade.regime` | 危険信号を評価した（毎朝） | `day`, `trade`, `reasons`, `month`, `iv_prev`, `drift_bp`, `market_gap_bp`, `recent_pnl`, `us_ret_bp`, `vix` |
| `daytrade.ranking` | ギャップ順の順位表（N と次点 5 件） | `day`, `n`, `budget`, `scale`, `weighting`, `quotes`, `rows`（`rank`, `symbol`, `gap`, `price`, `vol`, `quantity`, `picked`） |
| `daytrade.pick` | 買う銘柄を決めた（1 件ごと） | `day`, `symbol`, `code_`（J-Quants の 5 桁）, `rank`, `gap`, `prev_close`, `price`, `quantity`, `amount` |
| `daytrade.order` | 注文にした結果（1 件ごと。買いも売りも） | `day`, `symbol`, `side`, `client_order_id`, `quantity`, `price`, `amount`, `live`, `outcome`（`発注` / `dry-run` / `見送り 余力不足 …` / `見送り 締め切り …` / `見送り 余力を照会できない …` / `失敗 …`） |
| `daytrade.balance_failed` | 余力を照会できず、その銘柄だけ見送った（実行は止めない。次の銘柄へ進む） | `symbol`, `trade`, `error` |
| `daytrade.carry` | verify が売れ残り（持ち越し）を見つけた（通知も送る） | `day`, `positions`（銘柄と株数） |
| `daytrade.fill` | close / verify が注文をブローカーに照会した | `symbol`, `client_order_id`, `broker_order_id`, `before` / `after`（状態）, `quantity`, `filled`, `avg_fill_price`。照会できなければ warning で `after` が null |
| `daytrade.reconcile` | close / verify が台帳と食い違う建玉をブローカーに見つけた（通知も送る）／建玉を照会できなかった | `held`, `symbol` / `error` |
| `daytrade.pending_resolved` | 送信結果不明（`PENDING`）の注文を当日の注文一覧（銘柄・売買・区分・数量・時刻）で判定した。`outcome` = `attributed`（届いていた→注文番号と状態を帰属）/ `not_sent`（届いていない→`UNSENT`。同じ実行の中で種を変えて 1 度送り直す）/ `too_recent` | `day`, `client_order_id`, `symbol`, `side`, `quantity`, `outcome`, `reason`, `broker_order_id`, `status`, `filled` |
| `daytrade.pending_ambiguous` | 同じ銘柄・売買で数量か区分の違う未帰属の注文があり、自動で決められない（通知も送る）。`PENDING` のまま残り、その銘柄はその日は触らない。ダイジェストの `pending_ambiguous` に件数。AI はこの行だけで修復できる（`docs/FEEDBACK.md`「自己修復の手順」） | 同上＋ `placed_at`, `candidates[]`（`broker_order_id`, `quantity`, `trade`, `status`, `filled`, `created_at`）, `fix`（実行する `pending resolve` の雛形） |
| `daytrade.pending_unresolved` | 当日の注文一覧を照会できず判定を持ち越した。open は発注しない（次の cron で再判定） | `error` |
| `daytrade.skip` | 何もしなかった | `reason`（`disabled` / `holiday` / `window` / `regime` / `already` / `no_quotes` / `no_picks` / `no_capital` / `no_buys` / `nothing_to_sell`）と付随項目 |
| `daytrade.pnl_incomplete` | 資産曲線の評価で、売りの約定単価が確定していない日を除いた | `days` |
| `daytrade.iv_missing` / `daytrade.us_missing` | 前日の IV／前夜の米国市場を取れず、そのゲート無しで進んだ | `prev_day` / `error`, `source` |
| `daytrade.us_session` / `daytrade.us_warm` | 前夜の米国市場を読んだ（open）／前夜にキャッシュを温めた（plan）。`source` = `cache`（キャッシュに前日ぶんがあり取りに行かず）/ `fetched` / `cache_fallback`（取れずキャッシュの最新で代用） | `source`, `session` |
| `daytrade.run` | 実行の終了 | `phase`（`open` / `close`）, `live`, `reason`, `n`, `budget`, `picks` / `sells`, `failures`, `already_long` / `already_short`, `elapsed_ms`, `deadline` |
| `daytrade.evaluate` | 大引後に候補の全行へ日足を当てた（`docs/DAYTRADE.md`「候補の結果と選定の妥当性」） | `day`, `source`（`quotes` / `archive_open`）, `rows`, `picked`, `traded`, `path`, `summary`（脚 × 群の件数・平均 net bp・勝率・想定損益） |
| `daytrade.snap` | 板・気配をそのまま履歴に残した（`docs/OPENING_DATA.md`）。発注はしない | `day`, `slot`（JST の HHMM）, `scope`, `requested`, `rows`, `path` |
| `daytrade.crash` | 実行が例外で異常終了した（通知も送る）。exit 1 | `error`, `exception`（トレースバック） |

気配が取れなかった銘柄は `daytrade.quotes` の `missing_sample` に残る。

### 立花証券の電文（`broker.*`。daytrade / wbjp / accum で共通）

立花への電文は **1 本 1 行**で残る。パラメータ本体は残さない（発注パスワードが入る）。
「9:01:12 に CLMKabuNewOrder を送って 28 秒待った」を後から追うための行。

| `code` | いつ | 主な項目 |
|---|---|---|
| `broker.request` | 電文を送って応答を読めた（ログインも） | `clm`（電文の種類）, `iface`（`request` / `price` / `master` / `auth`）, `p_no`, `symbol`, `elapsed_ms`, `timeout_ms`, `http_status`, `p_errno`, `result_code`, `result_text`, `order_number` |
| `broker.request_failed` | 通信エラー・HTTP エラー・JSON でない応答・締め切りで送らなかった（warning） | 同上＋ `error`, `body`（応答本文の先頭 300 文字。メンテ画面等の切り分け） |
| `broker.retry` | 照会を送り直す（通信エラーで 1 度、セッション失効で 1 度）。新規注文は送り直さない | `clm`, `stage`（`login` / `send`）, `error` / `p_errno`, `backoff_ms` |
| `broker.order_number_missing` | 発注は受理されたのに注文番号が無い（以後照会・取消できない。error） | `client_order_id`, `symbol` |
| `broker.order_row_unreadable` / `broker.history_today_only` / `broker.lot_master_failed` | 注文一覧の 1 行を解釈できない／前日以前の注文は照会できない／売買単位のマスタを取れない（warning） | `order_number`, `error` / `start`, `today` / `error` |

### スイング売買（`wbjp`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `wbjp.signals` | 戦略が意見を出した | `strategy`, `signals`（件数） |
| `wbjp.pending_resolved` / `wbjp.pending_ambiguous` | daytrade と同じ。送信結果不明の注文を当日の注文一覧で判定した／決められなかった。決められないものがあれば `run` は発注せずに止まる（ダイジェスト `wbjp.pending_ambiguous`） | `client_order_id`, `symbol`, `side`, `quantity`, `outcome`, `reason` |

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
