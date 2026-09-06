# 改善ループ（判断 → 実績 → 改善）

運用の記録を、**AI が少ないトークンで読んで改善に使える形**にするための仕組み。
3 つの層でできている。上から順に見て、必要になったときだけ下に降りる。

| 層 | 何が分かるか | 置き場 | 大きさの目安 |
|---|---|---|---|
| 1. 日次ダイジェスト | 今日どの実行が動き、異常はあったか | `state/digest/<env>-<日付>.jsonl` | 数十 KB／日 |
| 2. 判断と実行品質の履歴 | 何を選び、想定とどれだけずれて約定したか | `state/<app>/history/<種類>/` | Parquet（列で絞れる） |
| 3. 構造化ログ | 再現に要る全部（[LOGGING.md](LOGGING.md)） | `state/logs/<app>-<env>.jsonl` | 数 MB／日 |

---

## 1. 日次ダイジェスト

各 CLI 実行が終わるときに、その実行を 1 行に畳んで追記する。**まずこれだけ読む。**

```console
# 今日の全実行
cat state/digest/prod-2026-09-03.jsonl

# 異常のあった実行だけ（ここに出ないものは深掘りしなくてよい）
jq 'select(.anomalies)' state/digest/prod-2026-09-03.jsonl
```

```json
{"app":"daytrade","command":"plan","run_id":"c07afa695dbc","outcome":"ok",
 "dur_ms":420,"candidates":3682,"eligible":952,"positions":3,
 "schema":"wbjp.digest.v1","ts_utc":"2026-09-02T11:30:03+00:00"}
```

- `outcome` は `ok` / `skip`（休日・時間帯の外）/ `error`
- `anomalies` は**いつもと違うことが起きた実行にだけ**付く（持ち越し、発注失敗、
  応答不明、J-Quants の欠け、異常終了）
- アプリを分けず 1 ファイルにするのは、「今日どう動いたか」を 1 回の読み込みで
  掴めるようにするため。1 実行 1 行の追記なので、cron のジョブが重なっても壊れない

深掘りするときは `run_id` を鍵に層 3 へ降りる（[LOGGING.md](LOGGING.md)）。

## 2-a. 実行品質（`state/<app>/history/execution/`）

**「そう判断した値」と「実際に約定した値」の差**を、上書きされない形で残す。

台帳（`state/*.db`）は最新の状態しか持たない。積立の `update_status` は約定額で
`amount` を上書きするので、事後には「判断時にいくらのつもりだったか」を復元できない。
改善に使いたいのはまさにその差分なので、別に追記専用の表を持つ。

1 回の発注は必ず 2 つの時点に分かれる（発注した瞬間に約定価格は分からない）。
1 行を書き換えるのではなく、`client_order_id` を鍵に行を足す:

| `event` | いつ | 主な列 |
|---|---|---|
| `intent` | 発注した時点 | `intent_price`, `intent_amount`, `intent_fee` |
| `fill` | 約定を照会して確定した時点 | `fill_price`, `fill_quantity`, `slippage_bp` |
| `skip` | 発注しなかった | `reason`（理由コード） |

`slippage_bp` は**有利ならプラス、不利ならマイナス**（買いは安く買えたら有利、
売りは高く売れたら有利——向きを揃えないと平均が意味を持たない）。

理由（`reason`）は `pkg/wbcore/execution` の `ReasonCode` の列挙。文言は変わるので、
集計は必ずコードで行う（人が読む説明は `note` に自由文のまま残る）。

```bash
# 行をそのまま見る
daytrade history execution --from 2026-09-01 --json

# 見送りの理由の分布
jquants query "SELECT reason, count(*) FROM read_parquet('state/daytrade/history/execution/*.parquet') WHERE event = 'skip' GROUP BY reason ORDER BY 2 DESC"

# 平均で何 bp 負けているか
jquants query "SELECT avg(slippage_bp) FROM read_parquet('state/daytrade/history/execution/*.parquet') WHERE event = 'fill'"
```

## 2-b. 判断の妥当性（`evaluate` / `review`）

「選んだものは、選ばなかったものより良かったか」を、3 つのアプリすべてで答える。
**比べる相手が要る**のが要点——採用したものの平均リターンだけを見ても、相場全体が
上がった日なら当然プラスになる。

| アプリ | 実績の測り方 | 対照群 | コマンド |
|---|---|---|---|
| `daytrade` | 当日の寄付 → 大引 | 次点（`next`）・候補全体（`rest`） | `daytrade evaluate` → `daytrade review` |
| `wbjp` | 判断日から `--horizon` 営業日後の終値 | 閾値超えだが枠外（`passed`）・圏外（`rest`） | `wbjp evaluate` → `wbjp review` |
| `accum` | 判断日から `--horizon` 営業日後の終値 | 倍率 1.0 の日（通常の積立） | `accum evaluate` |

いずれも**判断のロジックには触れない**。積んである判断履歴に後日の足を当てるだけ。
実績がまだ出ていない行は落とさず `null` のまま残す（落とすと「評価できたものだけ」の
偏った集計になる）。

積立だけ問いが違う。売らないので損益ではなく、**増額した日は本当に安かったか**を
倍率の帯ごとに見る。倍率 1.0 の帯が対照群になる。

## 3. `--json`

`review` / `evaluate` / `history` / `screen` には `--json` がある。付けると表を一切
出さず、標準出力に JSON を 1 個だけ書く。そのまま `jq` に渡せる。

```console
daytrade review --json | jq '.rows[] | select(.picked_bp < .all_bp)'
wbjp evaluate --json --horizon 20 | jq '.days'
accum evaluate --json | jq '.summary'
```

---

## 4. レポート（Discord）

上の 3 層を**定期的に AI に読ませ**、稼働・異常・判断の妥当性・気づき・改善案を
Discord に流す。人が毎日 `jq` を叩かなくても、崩れたときに気づけるようにするための仕組み。

日次・週次・月次の 3 つがある。日次は「今日どう動いたか」、週次と月次は**傾向**
（数字が伸びているか、選定が効き続けているか、同じ異常を繰り返していないか）を見る。

| 種類 | いつ | 対象 |
|---|---|---|
| 日次 | 平日 21:00 | その日 |
| 週次 | 金 21:30 | その週の月〜金。前週と比べる |
| 月次 | 毎月 1 日 22:00 | 前月まるごと。直近数か月の推移と、検証との乖離も見る |

| 部品 | 役割 |
|---|---|
| `.claude/agents/daily-report.md` | 日次のサブエージェント。**何をどの順で読むか**と、やってはいけないこと |
| `.claude/agents/periodic-report.md` | 週次・月次のサブエージェント。期間で絞って読む |
| `deploy/report.sh <daily\|weekly\|monthly>` | 起動と配達。期間に応じたサブエージェントを回し、標準出力を Discord に流す |
| `cmd/discord-post` | Discord の Bot API で送る。スレッドを作り、2000 文字で分割して連投する。実体は `pkg/wbcore/notify` |
| cron | 日次 `0 21 * * 1-5` / 週次 `30 21 * * 5` / 月次 `0 22 1 * *`（`deploy/crontab.txt`）。日次は evaluate 群と plan が出揃ってから |

```console
# 手で回す（送らずに中身だけ見る）
DRY_RUN=1 deploy/report.sh daily
DRY_RUN=1 deploy/report.sh daily 2026-09-02   # 日付を指定
DRY_RUN=1 deploy/report.sh weekly            # その週（月〜金）
DRY_RUN=1 deploy/report.sh monthly 2026-08   # その月
```

本文は送信の成否によらず `state/reports/`（`daily-<日付>.md` / `weekly-<開始日>.md` /
`monthly-<YYYY-MM>.md`）に残る。Discord に流したものの控えは `state/notify/<日付>.jsonl`（45 日）。

**生成と配達を分けてある。** モデルに「送る」ことまで任せると、送り忘れた日が
黙って消える。レポートを作るのは AI、Discord に届けるのは決め打ちのスクリプト。
生成に失敗した場合は、失敗した事実のほうが Discord に流れる。

**エージェントには読み取り専用のツールしか渡していない**（`tools: Bash, Read, Glob, Grep`、
起動時に `--disallowedTools Edit,Write,NotebookEdit`）。発注コマンドと `config/` の
書き換えは定義の中でも明示的に禁じてある——下の線引きと同じ理由。

**ダイジェストは起動時点の写しを読ませる。** エージェントが叩く `review` / `evaluate` は
それ自体がダイジェストに 1 行足すので、生のファイルを読ませると自分の足跡を
その日の運用実績として数えてしまう。

---

## 自動化するときの線引き

提案の生成（`evaluate` → `review` → `backtest` → `~/obsidian-vault/20-research/` への記録）までは
自動化してよい。**`config/*.toml` への反映は人が承認する。** パラメータの自動書き換えは
過学習と暴走がそのまま実弾に繋がるため。

### 送信結果不明の注文はプログラムが決める（人は見ない前提）

発注を送って応答が返らなかった注文（台帳の `PENDING`）は、人に「口座を確かめてください」
とは言わず、**当日の注文一覧（立花 CLMOrderList）と銘柄・売買・区分・数量・時刻で
突き合わせて自動で判定する**（`wbcore/reconcile`）。3 つの台帳とも同じ規則:

| 判定 | 台帳 | その後 |
|---|---|---|
| `attributed`（届いていた） | 注文番号と状態を帰属 | 通常どおり照会・手仕舞い |
| `not_sent`（届いていない） | `UNSENT`（終了状態） | 種を変えて送り直す。daytrade は同じ実行の中で 1 度、wbjp / accum は次の実行の差分で |
| `too_recent`（送った直後） | `PENDING` のまま | 次の実行で判定 |
| `ambiguous`（同じ銘柄で細部の違う未帰属の注文がある） | `PENDING` のまま | 通知＋ダイジェストの異常。daytrade はその銘柄を触らない、wbjp は発注を止める |

一覧を照会できないときは判定せず、実弾を出さない（`*.pending_unresolved`）。cron の次の回で再判定する。

ダイジェストには `pending_attributed` / `pending_unsent` / `pending_ambiguous` / `pending_too_recent` の
件数が載る。AI が動くのは **`pending_ambiguous` が 2 回以上の実行にまたがって続く**ときだけ。

### 自己修復の手順（AI が実行する）

`ambiguous` のログ行（`code` = `daytrade.pending_ambiguous` / `wbjp.pending_ambiguous` / `accum.pending_ambiguous`）には
判断に要るものが全部入っている——台帳側の注文（`client_order_id` / `symbol` / `side` / `trade` / `quantity` / `placed_at`）、
ブローカー側の未帰属の候補（`candidates[]`: `broker_order_id` / `quantity` / `trade` / `status` / `filled` / `created_at`）、
そして実行する修復コマンドの雛形（`fix`）。

```bash
# 1. 何が決められなかったか（run_id は digest の anomalies の行から）
jq -c 'select(.code | test("pending_ambiguous"))' state/logs/daytrade-prod.jsonl | tail -3

# 2. いま PENDING のまま残っているもの（3 アプリ共通の形）
bin/daytrade pending --json      # bin/wbjp pending --json / bin/accum pending --json

# 3. 候補と突き合わせて決める。届いていたなら注文番号を帰属、届いていないなら UNSENT
bin/daytrade pending resolve <client_order_id> --attribute <broker_order_id> --status FILLED --filled 100 --price 1234
bin/daytrade pending resolve <client_order_id> --unsent
```

判断の規則:
- 候補の `quantity` が台帳の `quantity` と一致し、`created_at` が `placed_at` の直後 → その候補に **帰属**
- 候補の数量が違い、`created_at` が `placed_at` より前 → 別の戦略（手動・別 cron）の注文。台帳の注文は **`UNSENT`**
- 候補のどれも説明がつかない → `UNSENT` にせず、その日のその銘柄は触らない（`PENDING` のまま翌日の verify で建玉を突き合わせる）

`pending resolve` は `PENDING` の行しか触らない（先に自動判定や約定が入っていれば拒否する）ので、
2 回実行しても壊れない。直したあとは通常の cron がそのまま続く——`UNSENT` なら次の実行で種を変えて送り直し、
帰属なら close / verify が注文番号で照会する。**daily-report サブエージェントは読むだけ**なので、
修復は Claude Code のセッション（人が起動する、または cron が `claude -p` で呼ぶ）が行う。
