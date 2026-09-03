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

## 4. 日次レポート（Discord）

上の 3 層を**毎営業日 21:00 に AI に読ませ**、稼働・異常・判断の妥当性・気づき・改善案を
Discord に流す。人が毎日 `jq` を叩かなくても、崩れたときに気づけるようにするための仕組み。

| 部品 | 役割 |
|---|---|
| `.claude/agents/daily-report.md` | サブエージェントの定義。**何をどの順で読むか**と、やってはいけないこと |
| `deploy/daily-report.sh` | 起動と配達。`claude -p --agent daily-report` を回し、標準出力を Discord に流す |
| `cmd/discord-post` | Webhook に POST する。2000 文字で分割する（Discord の上限）。実体は `pkg/wbcore/notify` |
| cron | `0 21 * * 1-5`（`deploy/crontab.txt`）。evaluate 群と plan が出揃ってから |

```console
# 手で回す（送らずに中身だけ見る）
DRY_RUN=1 deploy/daily-report.sh
DRY_RUN=1 deploy/daily-report.sh 2026-09-02   # 日付を指定
```

本文は送信の成否によらず `state/reports/daily-<日付>.md` に残る。

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

提案の生成（`evaluate` → `review` → `backtest` → `docs/research/` への記録）までは
自動化してよい。**`config/*.toml` への反映は人が承認する。** パラメータの自動書き換えは
過学習と暴走がそのまま実弾に繋がるため。
