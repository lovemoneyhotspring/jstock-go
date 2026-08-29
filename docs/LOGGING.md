# ログの形式（機械が読む用）

後から AI に読ませて改善に使う前提で、ログは **JSON Lines**（1 行 1 レコード、UTF-8）で
ファイルに残す。端末に出る整形済みの表示とは別の経路で、常に書かれる。

| 何 | どこ |
|---|---|
| ファイル | `data/logs/<app>-<env>.jsonl`（`WBJP_LOG_DIR` で変更可）。`app` は `wbjp` / `accum`、`env` は `uat` / `prod` |
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

### 積立（`accum`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `accum.fill` | 前回までに送った注文の約定状況が変わった | `symbol`, `client_order_id`, `before`, `after`, `filled`, `quantity`, `lost_ratio`（未約定のまま終わった割合。0 なら全部約定） |
| `accum.stale` | 足が古くて判定を見送った | `symbols`（銘柄 → 最終足の日付） |
| `accum.decision` | 今日出すべき投下を決めた | `symbol`, `market`, `month`（どの月の積立か）, `judged_on`（判断日）, `target`（今月の目標）, `placed`（発注済み）, `due`（差額＝今日出す額）, `multiplier`, `price`, `tactic`, `signal`（判定用の銘柄。無ければ null） |
| `accum.order` | 投下を注文にした結果（1 件ごと） | `symbol`, `client_order_id`, `quantity`, `price`, `amount`, `live`（実発注か）, `outcome`（`発注` / `dry-run` / `見送り …` / `失敗 …` / `発注済み（冪等）`）, `note`（見送り・失敗の理由） |
| `accum.run` | 実行の終了 | `live`, `reason`（dry-run の理由）, `orders`, `failures` |

金額・数量・価格は **文字列**（`"25000"`）で入っている。JSON の数値にすると Decimal の精度が失われるため。

### スイング売買（`wbjp`）

| `code` | いつ | 主な項目 |
|---|---|---|
| `wbjp.signals` | 戦略が意見を出した | `strategy`, `signals`（件数） |

`code` の無いログ（サイクル開始・発注・リスク判定の結果など）は `event` と付随項目で読む。
順次 `code` を付けていく。

## 読み方の例

1 回の実行を追う:

```bash
jq -c 'select(.run_id == "3f9a1c…")' data/logs/accum-prod.jsonl
```

積立の判断だけを表にする:

```bash
jq -r 'select(.code == "accum.decision") | [.ts_utc, .symbol, .month, .target, .placed, .due, .multiplier] | @tsv' data/logs/accum-prod.jsonl
```

未約定のまま終わった注文:

```bash
jq -c 'select(.code == "accum.fill" and (.lost_ratio | tonumber) > 0)' data/logs/accum-prod.jsonl
```

AI に渡すときは、対象の期間の行をそのまま渡せばよい。1 行が 1 レコードで自己記述的
（鍵の名前だけで意味が分かる）になっている。
