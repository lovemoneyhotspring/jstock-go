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

ダイジェストの `<日付>` は **JST**（`digest.DayOf`）。読む側の `deploy/report.sh`・
`night-repair.sh`・`morning-check.sh` が `TZ=Asia/Tokyo date +%F` で当日のファイルを選ぶので
それに揃えてある——UTC で切ると 09:00 JST より前の実行（寄り前の snap、6:20 の news、
6:00 の night-repair）が前日のファイルに入る。**2026-09-14 までのファイルは UTC の日付**で
切られている（移し替えていない）。その期間を読むときは、朝 9 時前の実行が前日のファイルに
入っていることに注意する。JSONL（層 3）の退避も 2026-09-21 から JST の日付（それまでは UTC）。
通知の控え `state/notify` も 2026-09-25 から JST の日付（**2026-09-24 までは UTC**。朝 9 時前の通知は前日のファイルにある）。

**どの置き場も日付でファイルが分かれている**ので、期間を指定すればその範囲の
ファイルだけを開く。全部読まずに済む構造が、記録が増えても壊れないための前提:

| 置き場 | 1 ファイルの単位 | 期間で絞る方法 |
|---|---|---|
| `state/digest/<env>-<日付>.jsonl` | 1 日（**JST** の日付） | ファイル名（`prod-2026-09-{01..30}.jsonl`） |
| `state/notify/<日付>.jsonl` | 1 日（**JST** の日付。2026-09-24 までは UTC） | ファイル名。Go からは `notify.ReadArchive(from, to)` |
| `state/logs/<app>-<env>.jsonl.<日付>` | 1 日（退避後） | ファイル名。当日分だけ接尾辞なし |
| `state/<app>/history/<種類>/*.parquet` | 1 実行（名前の先頭 10 文字が判定日） | `history.Store.Files(kind, Range)` が**開く前に**名前で選ぶ。SQL なら `WHERE day BETWEEN …` |
| `state/reports/*.md` | 1 レポート | ファイル名 |

保持: `state/logs/*.jsonl` の退避は 90 日（Go が消す）、`state/notify` は 45 日（Go が消す）、`state/digest` は 400 日（日曜の
`deploy/prune-state.sh` が消す）。cron の素のログ `state/logs/*.log` は 10MB を超えたら `.log.1` へ退避する（`docs/DEPLOY.md`「置き場の掃除」）。

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
| ローテーション | 日次（**JST の 0 時**。2026-09-20 までの退避は UTC の 0 時 = JST 9:00 で切られている）、90 日保持。ローテーション後は `…jsonl.YYYY-MM-DD`（日付は**中身が書かれた日**）。**日付ごとにファイルが分かれるので、他の日を読まずに済む**。退避はプロセスの起動時に行う（cron のプロセスはどれも短命なので、その日の最初の 1 本が退避する） |
| 文字 | UTF-8（日本語はそのまま） |
| 鍵の並び | 共通項目は固定の順（`schema`, `ts_utc`, `run_id`, `app`, `env`, `command`, `level`, `code`, `msg`, `extra`）、`extra` の中は辞書順 |
| 秘匿情報 | API キー・シークレット・口座 ID はファイルに書く前に `***` に置き換わる |

## 全レコードに付く項目

| 項目 | 意味 | 例 |
|---|---|---|
| `schema` | この形式の版（整数）。項目を変えたら上げる | `1` |
| `ts_utc` | 記録時刻（**常に UTC**、オフセット付き ISO 8601）。並べ替えと突き合わせの鍵 | `2026-08-29T07:32:39.123456+00:00` |
| `level` | `debug` / `info` / `warn` / `error` | |
| `msg` | 出来事の説明（日本語） | `休場日` |
| `run_id` | 1 回の CLI 実行の識別子。同じ実行のログを 1 本の線として読む | `3f9a1c…` |
| `app` | `wbjp`（スイング売買）/ `accum`（積立）/ `daytrade` / `jquants` | |
| `env` | `uat` / `prod` | |
| `command` | 実行したサブコマンド | `run` / `data sync` |
| `code` | 出来事の**安定した識別子**（付いているものだけ）。集計や分類はこれで行う | `accum.order` |
| `extra` | 出来事ごとの項目（下の「`code` の一覧」の「主な項目」はすべてこの中）。無ければ項目ごと出ない | `{"day":"2026-09-21","reason":"holiday"}` |
| `verify` | **実機検証で手で走らせた実行**（`--broker-verify`）。付いていない行は通常の運用 | `true` |

`msg` は人向けの文で、文言が変わることがある。分類には `code` を使うこと。

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

## 定型行（`routine`）

Go 版のログは `routine` を付けない（「動いただけ」の行も他と同じ形で残る）。2026-09 初めまでの古い
ファイル（Python 版の書式）にだけ `routine: true` の行があり、読み飛ばしてよい行を意味する。
`jq 'select(.routine != true)'` は新旧どちらのファイルにも使える。

## `code` の一覧

### J-Quants の蓄積（`jquants`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `jquants.ingest` | 端点 1 つ・対象 1 つを取り込んだ | `endpoint`, `target`（日付 / `bulk:<file>`）, `source`（`api` / `bulk`）, `rows`, `changed`（鍵で上書きして実際に変わった行数） |
| `jquants.gap` | `check` が営業日の欠けを見つけた | `missing` |
| `jquants.no_calendar` | 取引カレンダーが無く平日で代用した | — |
| `jquants.ingest_failed`（error） | 取り込み・一括ファイルの一覧の取得に失敗した | `endpoint`, `target`, `error` |
| `jquants.prune` | 取り込み済みの範囲を刈り込んだ（`jquants prune`） | `endpoint`, `target`, `windows`, `before` / `after`（行数）, `bytes` |

### 積立（`accum`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `accum.skip` | 何もしなかった（発注時間帯の外／銘柄ごとの見送り・発注済み（冪等）） | 本文に理由 |
| `accum.fill` | 前回までに送った注文の約定状況が変わった | 本文に銘柄・前後の状態・約定数量（`extra` は無い） |
| `accum.order` | 投下を注文にした結果（1 件ごと） | `symbol`, `client_order_id`, `quantity`, `price`, `amount`, `live`（実発注か）, `outcome`（`発注` / `dry-run` / `見送り …` / `失敗 …` / `発注済み（冪等）`）, `note`（見送り・失敗の理由） |
| `accum.unconfirmed`（error。exit 1） | 発注を送ったが応答が返らず、届いたか分からない。台帳には `PENDING` のまま残し、次の `run` の冒頭で当日の注文一覧と突き合わせて自動で判定する。**前日以前に送ったものは一覧に無くても「届いていない」とは決めず**（一覧が前日以前を返すか未確認）、`accum.unresolved` で保留して通知する。`PENDING` が残る銘柄には発注しない | 本文 |
| `accum.pending_resolved` | 送信結果不明の注文を当日の注文一覧で判定した（1 件ごと）。`attributed`（届いていた→注文番号を帰属）/ `not_sent`（届いていない→`UNSENT`、次の差額で埋め直す）/ `too_recent`（送った直後。次の run で） | 本文に銘柄・数量・判定 |
| `accum.pending_ambiguous` | ダイジェストの異常。送信結果不明（`PENDING`）の注文を決められず保留した。**前日以前に送ったものは次の run でも判定せず、`accum pending resolve` で確定するまで残る**（その銘柄は発注しない。`detail` に件数を分けて書く）。今日のもの（同じ銘柄で細部の違う未帰属の注文・一覧が 0 件）は次の run で再判定。続くなら口座の注文一覧を見る | `detail` |
| `accum.order` / `accum.dry_run` | 投下を注文にした結果（1 件ごと。実発注／dry-run）。判断そのものは `state/accum/history/decision/` に残る | 本文に銘柄・株数・価格・注文 ID |
| `accum.order_failed`（error。ダイジェストの失敗・通知・exit 1） | 出すべきなのに出せなかった銘柄（足が無い・古い・判定用の足が読めない・売買単位が分からない（`lot_size_overrides` にもブローカーの銘柄情報にも無い。dry-run は銘柄情報を引かない）・売買単位が設定と銘柄情報で違う・見積り失敗・買付余力不足・拒否・送信結果不明の注文が残る）。1 件ごとの行と、回の最後にまとめた 1 行 | 本文に銘柄と理由 |
| `accum.order_not_recorded` / `accum.order_aborted`（error。exit 1） | ブローカーは受理したのに台帳を更新できなかった・拒否されたのに台帳を `REJECTED` にできなかった（台帳は `PENDING` のまま。次の run の照合に回る）／送る前の台帳の記録に失敗した。どれも以降の発注を止める | 本文 |
| `accum.plan_failed`（error。exit 1） | 台帳（発注済み額・開始日）が読めず、発注計画を立てなかった | 本文 |
| `accum.balance_failed`（warn） | 買付余力を照会できず、その回は発注しなかった | 本文 |
| `accum.unrecorded_fills`（error） / `accum.unrecorded_check_failed` | 台帳に無い当月の約定がある（二重買付の恐れ。発注しない）／その照会ができなかった | 本文 |
| `accum.stale_signal`（warn） | 判定用の足が古いまま前日以前の値で判定した | 本文 |
| `accum.unresolved`（warn。ダイジェストの異常にも） | 注文を照会できず保留した（ログは保留した全件。ダイジェストの異常は `PENDING` 以外＝照会できない・応答に無い注文の件数）。通知は 1 回の run に 1 通で、前日以前の `PENDING` を含めば件名で `accum pending resolve` が要ると言う。今日の注文が立って発注できなかった銘柄（`accum.order_failed`）で知らせた注文は、この通知から外す | 本文 |
| `accum.sync_failed` / `accum.backup_failed`（ダイジェストの異常） / `accum.history_failed` / `accum.ledger` / `accum.ledger_read_failed` | 足の同期・注文状態の照会の失敗／バックアップの失敗／判断履歴・台帳への書き込み・読み込みの失敗 | 本文 |
| `accum.import_fill` / `accum.import_no_price` | `accum import-fills` が約定を取り込んだ／単価が取れず取り込めなかった | 本文 |
| `accum.verify_order` / `accum.verify_dry_run` / `accum.verify_query_failed` / `accum.verify_stop` / `accum.verify_stop_dry_run` / `accum.verify_stop_cancel_failed` | 発注経路の検証（`verify-order`・逆指値の検証。[BROKER_VERIFY.md](BROKER_VERIFY.md)）の結果 | 本文 |
| `accum.timezone`（warn） | `WBJP_TIMEZONE` を解釈できず、表示は既定の日本時間のまま（ファイルの `ts_utc` は常に UTC） | 本文 |
| `accum.crash` | 実行が例外で異常終了した（通知も送る）。exit 1 | `error`。panic は `<app>.panic`（`error`, `stack`） |

金額・数量・価格は **文字列**（`"25000"`）で入っている。JSON の数値にすると Decimal の精度が失われるため。

### デイトレ（`daytrade`）

不具合の再現に要る 4 つ——**そのとき有効だった設定**（`daytrade.config`）、**入力**（`daytrade.plan` / `daytrade.quotes` /
`daytrade.regime`）、**判断**（`daytrade.ranking` / `daytrade.skip`）、**結果**（`daytrade.order` / `daytrade.fill` /
`daytrade.run` / `daytrade.crash`）——を毎回の実行に残す。1 回の実行は `run_id` で束ねる。

ログは再現用で、順位表は上位 N+5 件、保持は 90 日。振り返り・検証のための**全行**（候補・気配・順位表・実行の要約）は
`state/daytrade/history/` に追記専用の Parquet で残し、同じ `run_id` で突き合わせられる（`docs/DAYTRADE.md`「履歴」）。
`wbjp screen` も同様に `state/wbjp/history/screen/` に残す。

| `code` | いつ | 主な項目 |
|---|---|---|
| `daytrade.config` | 実行の冒頭（plan / open / close） | `phase`, `day`, `live`, `enabled`, `max_capital`, `positions`（N）, `budget_per_order`, `segments`, `min_turnover`, `max_gap`, `skip_months`, `iv_gate`, `drift_gate`, `equity_curve_days`, `us_skip`, `quote_source`, `entry_window` / `exit_window`, `max_quote_age`, `kill_switch`, `watch_only`, `state_dir`, `data_dir`, `deadline`（この実行の締め切り、JST）, `max_run_seconds` |
| `daytrade.resume` | 同じ日の前の回が建てた建玉があり、残りの枚数だけ建て直す | `long`, `short`（建てた数）, `remaining_long`, `remaining_short`, `symbols`（建てた銘柄。候補から外す） |
| `daytrade.plan` | 前夜に候補を作った | `day`, `prev_day`, `candidates`（全銘柄）, `eligible`（対象）, `positions`, `budget`, `iv_prev`, `path` |
| `daytrade.quotes` | 気配を取った／使えない気配を除外した | `source`, `requested`, `received`, `missing`, `missing_sample`（取れなかった銘柄、最大 20）, `elapsed_ms`, `from_book`（値段を板＝最良気配から取った銘柄の数。まだ寄っていない銘柄で、利益源はここに集まる）/ `stale`, `stale_sample`（`銘柄@時刻 年齢秒`）, `delayed`, `delayed_sample`, `future`（時刻が 1 分以上先の気配の数。寄り前に前日の時刻が返る罠の観測用）, `future_sample`, `max_age_sec` |
| `daytrade.regime` | 危険信号を評価した（毎朝） | `day`, `trade`, `reasons`, `month`, `iv_prev`, `drift_bp`, `market_gap_bp`, `recent_pnl`, `us_ret_bp`, `vix` |
| `daytrade.ranking` | 順位表（N と次点 5 件） | `day`, `n`, `budget`, `scale`, `weighting`, `quotes`, `rows`（`rank`, `symbol`, `gap`, `price`, `vol`, `quantity`, `picked`。`rank_by = "lgbm"` の日は `score`（予測値）と `rule_rank`（gap_vol での順位）も） |
| `daytrade.rerank`（warn） | `rank_by = "lgbm"` なのに LightGBM で並べられない（plan が古い版・モデルが読めない・当日の気配で試しに並べて失敗）。その回は gap_vol で並べ、`us_skip_legs` を `all` に戻す（米国小幅高の日は両脚とも休む）。異常（digest）にも載る | `rank_by`, `fallback`, `reason`, `plan_features`, `us_skip_legs`（寄付の判定の後に失敗した回は `error`, `short_off`。`short_off` が真ならロングも建てない） |
| `daytrade.order` | 注文にした結果（1 件ごと。買いも売りも） | `day`, `symbol`, `side`, `client_order_id`, `quantity`, `price`, `amount`, `live`, `outcome`（`発注` / `dry-run` / `見送り 余力不足 …` / `見送り 締め切り …` / `見送り 余力を照会できない …` / `失敗 …`） |
| `daytrade.balance_failed` | 余力を照会できず、その銘柄だけ見送った（実行は止めない。次の銘柄へ進む） | `symbol`, `trade`, `error` |
| `daytrade.carry` | verify が売れ残り（持ち越し）を見つけた（通知も送る） | `day`, `positions`（銘柄と株数） |
| `daytrade.overclosed`（error） | verify が、手仕舞いの約定が建玉の約定を超えた脚を見つけた（保険の引け注文と 15:20 の成行が両方約定した、など。通知も送る）。超えた分は反対の建玉か、別口（積立）の現物の売りになっている。持ち越しの判定とは別に出す | `day`, `positions`（銘柄・建玉と手仕舞いの株数・超過） |
| `daytrade.fill` | close / verify が注文をブローカーに照会した | `symbol`, `client_order_id`, `broker_order_id`, `before` / `after`（状態）, `quantity`, `filled`, `avg_fill_price`。照会できなければ warning で `after` が null |
| `daytrade.reconcile` | close / verify が台帳と食い違う建玉をブローカーに見つけた（通知も送る）／建玉を照会できなかった | `held`, `symbol` / `error` |
| `daytrade.pending_resolved` | 送信結果不明（`PENDING`）の注文を当日の注文一覧（銘柄・売買・区分・数量・時刻）で判定した。`outcome` = `attributed`（届いていた→注文番号と状態を帰属）/ `not_sent`（届いていない→`UNSENT`。同じ実行の中で種を変えて 1 度送り直す）/ `too_recent` | `day`, `client_order_id`, `symbol`, `side`, `quantity`, `outcome`, `reason`, `broker_order_id`, `status`, `filled` |
| `daytrade.pending_ambiguous` | 同じ銘柄・売買で数量か区分の違う未帰属の注文があり、自動で決められない（通知も送る）。`PENDING` のまま残り、その銘柄はその日は触らない。ダイジェストの `pending_ambiguous` に件数。AI はこの行だけで修復できる（`docs/FEEDBACK.md`「自己修復の手順」） | 同上＋ `placed_at`, `candidates[]`（`broker_order_id`, `quantity`, `trade`, `status`, `filled`, `created_at`）, `fix`（実行する `pending resolve` の雛形） |
| `daytrade.pending_unresolved` | 当日の注文一覧を照会できず判定を持ち越した。open は発注しない（次の cron で再判定）。close は当日の手仕舞いを続ける（判定できなかった建玉は `daytrade.unconfirmed` に出る） | `error` |
| `daytrade.exit_refresh` | close の 2 回目以降（15:24・15:28）が、前の回に受理された手仕舞いを照会できなかった、または照会の約定数量が台帳より少なかった（書き戻さない）。発注済みとして数えたまま（重ねて出さない）。残っていれば 15:40 の verify が持ち越しとして知らせる | `symbol`, `client_order_id`, `error` |
| `daytrade.skip` | 何もしなかった | `reason`（`disabled` / `holiday` / `half_day` / `no_calendar` / `window` / `regime` / `already` / `no_quotes` / `no_picks` / `no_capital` / `no_buys` / `nothing_to_sell`）と付随項目 |
| `daytrade.half_day`（warn） | 判定日が半日立会（カレンダーの `HolDiv = 2`）。手仕舞い（`exit_window`、15:20〜）の前に引けるので `open` は建てない（`daytrade.skip` の `half_day`、warn）。前夜の `plan` がこのコードと運用通知を 1 回出す。手仕舞う側（`close`・`guard`・`protect`）は止めない。今の東証に半日立会は無い | `day` |
| `daytrade.opening_unfilled` | 寄付条件の建て注文が約定せずに終わった（寄指の指値に届かなかった、または 1 日寄らなかった）。`close` が建て注文を確定した後に出す。**失敗ではない**——枠が現金で残った日の記録 | `count`, `symbols` |
| `daytrade.opening_limit`（warn） | 米国小幅高の日を寄指だけで取引する設定（`execution.preopen_limit_pct_us_low`）で、寄指の指値を作れない銘柄（前日終値が無い・呼値に丸められない）を発注から外した。平常日は寄成に戻すが、この日の寄成は損の側なので出さない。外した件数は `open_run` の `opening_limit_dropped` | `symbol` / `side` / `prev_close` |
| `daytrade.calendar`（warn） | 取引カレンダーが空（取り込みが無い・壊れた）。発注する回（`--live`）は休場か分からないので見送る（`daytrade.skip` の `no_calendar`）。発注しない回は平日で代用して続ける | `phase`, `day`, `live` |
| `daytrade.signal`（warn） | 打ち切り（SIGTERM。`deploy/with-lock.sh` の時間切れ）を受け、実行品質（滑り）の記録を書き出して終了した | `signal` |
| `daytrade.pnl_incomplete` | 資産曲線の評価で、売りの約定単価が確定していない日を除いた | `days` |
| `daytrade.iv_missing` / `daytrade.us_missing` | 前日の IV／前夜の米国市場を取れず、そのゲート無しで進んだ | `prev_day` / `error`, `source` |
| `daytrade.us_session` / `daytrade.us_warm` | 前夜の米国市場を読んだ（open）／キャッシュを温めた（朝の warm-us・前夜の plan。`ready` = 判定日の前夜の行が VIX つきで入った。plan では常に偽。warm-us で偽なら warn「前夜の米国市場がまだ揃っていない」が続く）。`source` = `cache`（キャッシュに前日ぶんがあり取りに行かず）/ `fetched` / `cache_fallback`（取れずキャッシュの最新で代用）/ `cache_no_vix`（前夜の S&P500 はあり、VIX だけどこからも取れない。同じ VIXCLS を取り直して待ちを倍にしないよう、その回はここで打ち切る） | `source`, `session` |
| `daytrade.us_vix_missing` | S&P500 は取れたが **VIX だけ**取れていない。`us_vix_override`（VIX > 24 ならゲートを無視）の例外が効かず、「小幅高で休む」が**休む側に倒れる**。取引そのものは止めない | `session`, `source` |
| `daytrade.run` | 実行の終了 | `phase`（`open` / `close`）, `live`, `reason`, `n`, `budget`, `picks` / `sells`, `failures`, `already_long` / `already_short`, `elapsed_ms`, `deadline` |
| `daytrade.rule_compare` | LightGBM で並べた日の、ロングの実際の選定と既存規則（gap_vol）の選定の比べ（evaluate の後）。**円で比べるのは `*_even_pnl`**（1 注文 = 予算の等金額に揃えた想定損益）——`*_hypo_pnl` は LightGBM 側だけ按分の株数なので引き算できない | `day`, `lgbm_picks`, `lgbm_avg_net_bp`, `lgbm_hypo_pnl`, `lgbm_even_pnl`, `rule_picks`, `rule_avg_net_bp`, `rule_hypo_pnl`, `rule_even_pnl`, `overlap`, `skipped`, `rule_off`（既存規則なら建てない日＝米国小幅高。gap_vol 側 0 件が正しい姿）, `later_runs`（比べから外した「後の回で建てた」件数） |
| `daytrade.evaluate` | 大引後に候補の全行へ日足を当てた（`docs/DAYTRADE.md`「候補の結果と選定の妥当性」） | `day`, `source`（`quotes` / `archive_open`）, `rows`, `picked`, `traded`, `path`, `summary`（脚 × 群の件数・平均 net bp・勝率・想定損益） |
| `daytrade.ranking_run` | 評価に使う順位表の回を選んだ。`open` は 9:01〜9:13 に何度も走り、**建て終わった後の回は建玉を候補から外すので picked が 0 になる**。最後の回をそのまま使うと選定が丸ごと評価から抜けるため、**picks のある最初の回**を選ぶ（2026-09-16 に実際に起きた）。picks のある回が無い日（本当の見送り）は最後の回のまま。**危険信号で見送った回は「picks のある回」に数えない**——見送りの回にも「建てていたら」の picked が立つので、前半が見送りで後半に建てた朝（2026-09-15）に 1 株も建てていない仮想の回を採ってしまう | `day`, `chosen`（採った回の `recorded_at`）, `runs`（その日の回数）, `picked`, `fallback`（picks のある回が無く最後の回を使った）, `skipped`（採った回が見送りの回＝本当の見送り日） |
| `daytrade.preflight` | 8:40 の寄る前の点検（`daytrade preflight`）。問題なしは info、問題があれば error（ダイジェストの異常にも。自動復旧 `deploy/guard-preopen.sh` が読む） | `day`, `problems`（文字列の配列。種別は `config` / `plan` / `ledger` / `disk`） |
| `daytrade.plan_missing`（異常） | 朝の時点で当日の plan が無かった（6:40 の `plan --if-missing` が作る） | ダイジェストのみ |
| `daytrade.us_stale`（warn。異常にも） | 前夜の米国市場が取れないまま判定した | `want` |
| `daytrade.no_quotes`（異常） | 気配が 1 銘柄も取れず、その回は何もしなかった | ダイジェストのみ |
| `daytrade.order_failed`（異常） / `daytrade.exit_failed`（異常） | 建玉／手仕舞いが通らなかった件数（通知も送る） | ダイジェストのみ |
| `daytrade.unrecorded_positions`（error。異常にも） / `daytrade.unrecorded_check_failed`（error） | 台帳に無い建玉がある（二重に建てる恐れ。発注を中止し通知）／建玉を照会できず中止した | `day`, `positions` / `error` |
| `daytrade.sweep`（warn / error。異常にも） / `daytrade.sweep_check_failed`（error） | 台帳に無い信用建玉を返済した／それを判定するための建玉照会ができなかった。`open` は返済に回した銘柄を今日の候補から外す（warn） | `day`, `symbol`, `leg`, `quantity`, `held`, `recorded` / `symbols` |
| `daytrade.carry_failed` / `daytrade.carry_unconfirmed` / `daytrade.carry_check_failed`（error） | 持ち越しの手仕舞いが通らなかった／注文を照会できず持ち越しを判定できない／持ち越しを判定できず当日の手仕舞いだけ行った（`open` の冒頭。`daytrade.carry` の異常の側） | `day`, `error` |
| `daytrade.unconfirmed_entries`（異常） | `close` が建て注文の取消・照会を確かめられず、建玉が残っていないか口座の確認が要る | ダイジェストのみ |
| `daytrade.entry_cancel`（warn） | `close` が板に残った建て注文の取消の完了を確かめられない。取消の電文がエラーだったときも同じ code で「取消がエラー。照会し直して決める」（`daytrade.protect_cancel`・`daytrade.corp_guard` も、それぞれ保険の引け注文・材料の出た売建の取消で同じ形） | `day`, `symbol`, `client_order_id`, `status`, `filled`, `quantity` / `error` |
| `daytrade.ledger`（error） | 台帳への書き込みに失敗した（未送信・拒否・受理後の更新・約定状況）。受理後の更新失敗は送信結果不明として扱う | `client_order_id`, `symbol`, `error` |
| `daytrade.ref_price`（info / warn） | 執行時の時価（滑りの基準）を選定の気配で代用した／まとめて取れず記録を諦めて発注を優先した | `age_ms`, `reused`, `picks` / `day`, `symbol(s)`, `error` |
| `daytrade.execution` / `daytrade.history` / `daytrade.evaluate_unmatched`（warn） | 実行品質の記録・履歴の追記に失敗した／順位表に無い約定があり評価から落ちる | `error` / `kind`, `error` / `day`, `orders` |
| `daytrade.sector`（warn） | 業種が取れない候補があり `max_per_sector` がその分効かない | `no_sector`, `ranked`, `max_per_sector` |
| `daytrade.margin_warm`（info / warn） | 8:53 の `warm-margin` が委託保証金をキャッシュに焼いた／取れなかった・応答に無い項目・不足額（追証。通知も送る） | 保証金の内訳（`sonota_kousokukin` など） |
| `daytrade.margin_cap`（warn / error） | `open` が保証金で建玉の上限を導いた。キャッシュが無い／当日ぶんでないときは設定の値で建てる（warn）、下げた設定が検証を通らない（error。通知も送る） | 導いた上限、`cached_day` |
| `daytrade.corp_event` / `daytrade.corp_note` | ニュースの材料でショートの対象から外した（TOB など）／記録だけで外さなかった（`open`・`plan`） | `symbol`, `name`, `kind`, `at`, `headline` |
| `daytrade.corp_guard`（info / warn / error） | `guard` が材料の出た売建を見つけた／取消・返済した／処置できなかった（`daytrade.corp_guard_failed` は異常。通知も送る） | `symbol`, `kind`, `at`, `headline` |
| `daytrade.news_stale`（error / warn。異常にも） | ニュースの記録簿を読めない（売建の点検ができない。通知も送る）／古いまま点検した | `error` / `reason`, `phase` |
| `daytrade.protect`（info） | 保険の引け注文の結果（`daytrade protect`。1 件ごと） | `day`, `symbol`, `quantity`, `outcome` |
| `daytrade.protect_failed`（異常） / `daytrade.protect_filled_early`（warn。異常にも） / `daytrade.protect_held`（warn） | 保険の引け注文が通らなかった／引けの前に約定・失効していた（実機の挙動を確かめる）／取り消せず、その株数は引け値の手仕舞いに任せる | `day`, `orders` / `held` |
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
| `broker.retry` | 照会を送り直す（通信エラーで 1 度、セッション失効 `p_errno=2` で 1 度）。新規注文は送り直さない。`-1`（引数エラー）・`-62`（時間外）はセッションを捨てず送り直さない | `clm`, `stage`（`login` / `send`）, `error` / `p_errno`, `backoff_ms` |
| `broker.warning` | 応答に `sWarningCode` が付いた（受理されたうえでの注意書き。warning） | `clm`, `iface`, `p_no`, `warning_code`, `warning_text`, `order_number` |
| `broker.duplicate_order` | 同じプロセスで同じ `client_order_id` を既に出していたので送らなかった（warning） | `client_order_id`, `broker_order_id`, `symbol` |
| `broker.order_number_missing` | 発注は受理されたのに注文番号が無い（以後照会・取消できない。error） | `client_order_id`, `symbol` |
| `broker.order_row_unreadable` / `broker.history_today_only` / `broker.lot_master_failed` | 注文一覧の 1 行を解釈できない／前日以前の注文は照会できない／売買単位のマスタを取れない（warning） | `order_number`, `error` / `start`, `today` / `error` |

### スイング売買（`wbjp`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `wbjp.pending_resolved` / `wbjp.pending_ambiguous` | daytrade と同じ。送信結果不明の注文を当日の注文一覧で判定した／決められなかった。決められないものがあれば `run` は発注せずに止まる（ダイジェスト `wbjp.pending_ambiguous`） | `client_order_id`, `symbol`, `side`, `quantity`, `outcome`, `reason` |
| `wbjp.pending_too_recent`（ダイジェストの異常。通知も送る） | 判定が `too_recent`（注文一覧が空・反映待ちで信用できない、または送った直後）の注文があり、`run` は発注せずに止まった。wbjp の注文 ID は株数から作るので、9:31 と 13:31 で株数が変わると `WasPlaced` では二重建てを防げない。次の回で判定し直す | 本文 |
| `wbjp.order` / `wbjp.dry_run` / `wbjp.skip` / `wbjp.risk_rejected`（warn） / `wbjp.reconcile_skip` | 発注した／dry-run／既に発注済み（冪等）／リスク判定で見送った／建玉の突き合わせで見送った（1 件ごと） | 本文に銘柄・売買・株数・理由 |
| `wbjp.order_failed`（error。ダイジェストの異常にも） / `wbjp.unconfirmed`（error） | 発注を拒否された／送信結果が分からない（台帳は `PENDING`） | 本文 |
| `wbjp.fill` / `wbjp.fill_unresolved`（warn。ダイジェストの異常にも） / `wbjp.fill_sync_failed`（異常） | 約定状況が変わった／注文を照会できず台帳が未確定／約定の同期に失敗 | 本文 |
| `wbjp.pending_unresolved`（異常） | 当日の注文一覧を照会できず判定を持ち越した | `error` |
| `wbjp.pending_stale`（warn。ダイジェストの異常にも） | 今日より前に送った送信結果不明（`PENDING`）の注文がある。立花の一覧は当日分しか返らないので自動では判定しない。放っておくと買いは未約定の枠を押さえ続ける。口座の約定履歴を見て `wbjp pending resolve <id> --attribute <注文番号> --status FILLED --filled <株数> --price <単価>` か `--unsent` で確定する（ID に発注日が入るので `--unsent` でも送り直しは起きない） | `pending`, `client_order_ids`, `fix` |
| `wbjp.positions_empty`（発注する回はダイジェストの異常。通知も送る） | 建玉の照会がエラーなしで 0 件なのに、台帳では保有中のはずの銘柄（保存済みのストップ、前に成功した発注する回以降に約定した・約定が分からない買い。同じ期間に売った銘柄は除く）がある。信じるとストップを全部消して買い直すので、発注する回は止まる。dry-run は warn だけで続く（建玉 0 の模型で判断する）。口座を確かめ、本当に空（手で全部売った など）なら `wbjp run --live --yes --accept-flat` を 1 回走らせる（台帳のストップが外れ、その回が次の基準になる） | 本文に銘柄と根拠 |
| `wbjp.bars_unusable`（warn。ダイジェストの異常にも。保有中の銘柄があれば通知も送る） / `wbjp.calendar_missing`（warn） / `wbjp.market_closed` | 足が古い・読めない銘柄があり、その銘柄はこの回に売りも買いも出さない（保有中なら**損切り・利確・時間切れも止まる**）／取引カレンダーが読めない（dry-run は平日で代用、発注する回は止まる）／休場日のため判断しない | 本文に銘柄・保有株数・理由 |
| `wbjp.daily_pnl` / `wbjp.daily_pnl_unknown`（warn。ダイジェストの異常にも） | 当日の損益（実現・含み）と `max_daily_loss`／当日の損益を確かめられず新規の買いを止めた | 本文 |
| `wbjp.regime` / `wbjp.strategy_error`（warn） / `wbjp.margin_missing`（warn） | 相場の状態を評価した／戦略の評価に失敗した／信用残がアーカイブに無い | 本文 |
| `wbjp.outside_universe`（ダイジェストの `outside_universe` にも） | ユニバース（`universe.symbols`）外の保有（手で買った株など）がある。wbjp は目標を作らず売りも買いも出さない（地合いの手仕舞いも掛けない。`wbjp.reconcile_skip` にも理由が出る）。ユニバースから外した銘柄を自動で手仕舞う仕組みは無いので、外す前に手で売るか、売り切るまでユニバースに残す | 本文に銘柄 |
| `wbjp.stop_exit`（warn） / `wbjp.stop_removed` / `wbjp.stop_save_failed`（warn） | ストップに掛かった／保有の無い銘柄のストップを外した／ストップを保存できない | 本文 |
| `wbjp.ledger`（warn / error） | 台帳（実行・建玉・シグナル・未送信・拒否）への書き込みに失敗した | 本文 |
| `wbjp.crash` | 実行が例外で異常終了した（通知も送る） | `error` |

### 通知（`notify.*`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `notify.skipped`（warn） | 通知先（チャンネル ID）が未設定で送らなかった | 本文に理由・タイトル・本文 |
| `notify.failed`（error） | Discord への送信に失敗した | 本文 |

コードに無い `code` は付いていない行もある（サイクルの進行など）。その行は `msg` と `extra` で読む。

## 読み方の例

1 回の実行を追う:

```bash
jq -c 'select(.run_id == "3f9a1c…")' state/logs/accum-prod.jsonl
```

積立の発注だけを表にする（判断の中身は `state/accum/history/decision/`）:

```bash
jq -r 'select(.code == "accum.order") | [.ts_utc, .msg] | @tsv' state/logs/accum-prod.jsonl
```

異常（error）だけ:

```bash
jq -c 'select(.level == "error")' state/logs/accum-prod.jsonl
```

AI に渡すときは、対象の期間の行をそのまま渡せばよい。1 行が 1 レコードで自己記述的
（鍵の名前だけで意味が分かる）になっている。
