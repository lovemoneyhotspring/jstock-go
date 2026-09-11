# レーティングの蓄積（`rate`）

証券会社の投資判断・目標株価の一覧（グレイル NET）を定期的に取り、
**どの証券会社がいつ出すか**を後から測れる形で溜める。

## 何が測れて、何が測れないか

一覧ページに**時刻は載っていない**。載っているのは掲載日（MM/DD）だけ。
そこで短い間隔で取りに行き、**その行を初めて見た時刻**（`first_seen_at`）を掲載時刻の代わりにする。

- 時刻の粗さ = 取りに行く間隔。5 分おきなら ±5 分。
- 溜め始めた最初の 1 回は 1 か月ぶんがまとめて入る。この行は時刻を持たないので `timing` の集計から外している。
- ページが持つのは直近 1 か月ぶん（約 28 営業日・2000 行）だけ。**止めた期間は取り返せない。**
- 取れなかった回も `fetches` に残す。「取れなかった時間帯」と「載っていなかった時間帯」を混ぜないため。

## 三つの取得元

| | `rate sync`（立花証券 API） | `rate history`（トレーダーズ・ウェブ） | `rate fetch`（グレイル NET） |
|---|---|---|---|
| 中身 | **変更だけ**（上げ・下げ・新規） | 据え置きも含む全行 | 据え置きも含む全行 |
| 遡れる範囲 | 90 日 | **2005 年〜** | 1 か月 |
| 時刻 | 07:10（前営業日ぶんの一括） | なし（大引け後に更新） | なし（初出時刻で代用） |
| 銘柄コード | ○ | ○（市場区分も） | ×（銘柄名だけ） |
| 上げ下げの判定 | 配信元が分けている | **序列表に当てて出す**（下記） | 「格上げ」「格下げ」の語がある |
| 表 | `rating_events` | `traders_ratings` | `ratings` |
| 規約 | 自分の口座の正規 API | **転用・複製を禁止と明記**。外に出さないこと | 記載なし |

出所ごとに表を分けている。混ぜると「どこまで信じてよいか」が行ごとに変わってしまう。

**ルックアヘッドに注意。** 立花の `rating_events` は `feed_date`（知れた日）を持つので
それ以降に使えば安全。トレーダーズの `traders_ratings` が持つのは**掲載日**で、
掲載は大引け後なので、**翌営業日から使う**こと。

取得元の比較は `~/obsidian-vault/20-research/2026-09-rating-data-sources.md`。

### 上げ下げの判定（トレーダーズ）

トレーダーズの表記は `Buy→Hold` のように前後が並ぶだけで「格上げ」「格下げ」の語が無い。
そこで `pkg/rate/direction.go` の序列表に当てて向きを出し、`direction` 列に入れている。

- 数値の判断（`2→1`）は **1 が最上位**なので、数が小さくなれば格上げ。`2+` は半段階
- `Buy2` の末尾の数字は UBS 形式の**リスク区分**で、判断の強さではないので落とす
- `Buy→2` のように方式が変わった行は比べようがないので `unknown`。**`keep` に倒さない**
- 序列表を直したら `bin/rate direction` で全行を計算し直す

## 使い方

```bash
bin/rate sync                       # 立花 API から過去 90 日のレーティングの動きを取り込む
bin/rate history --from 200501      # トレーダーズ・ウェブの月別一覧を取り込む（2005 年〜）
bin/rate direction                  # 上げ下げを計算し直す（序列表を直したとき）
bin/rate fetch                      # グレイルを取って、初めて見た行を足す（cron から呼ぶ）
bin/rate latest --limit 40          # 最近記録した行を初出の順に
bin/rate latest --action up,down    # 格上げ・格下げだけ
bin/rate timing --days 90           # 証券会社ごとの出す時間帯
bin/rate query "SELECT ..."         # 直接 SQL
```

記録簿は `data/rate/rate.db`（SQLite）。`--db` で変えられる。

`timing` の時刻は**掲載日 0 時からの経過**で出る。前夜〜早朝に載ったぶんは 24 時超えになる。
`--max-lag`（既定 36 時間）より遅く見えた行は取り込み漏れの後追いとみなして外す。

## cron

朝方に集中するので、その帯を細かく、日中は粗く見る。

```cron
# グレイルの初出時刻を測る（朝方を細かく、日中は粗く）
*/5 5-10 * * 1-5  cd ~/jstock-go && bin/rate fetch --quiet >> state/logs/rate.log 2>&1
*/20 11-23 * * *  cd ~/jstock-go && bin/rate fetch --quiet >> state/logs/rate.log 2>&1
# 立花 API の動きを日次で足す（07:10 配信なので 07:30 以降）
30 7 * * 1-5      cd ~/jstock-go && bin/rate sync --days 5 >> state/logs/rate.log 2>&1
# トレーダーズの当月を取り直す（大引け後に更新されるので夜）
0 20 * * 1-5      cd ~/jstock-go && bin/rate history --from $(date +%Y%m) --force >> state/logs/rate.log 2>&1
```

**`sync` は毎日回す。** 90 日を過ぎたぶんは取り返せない。

`history` は 1 か月ごとに 5 秒空ける。短すぎると 429 を返される（1.5 秒で止められた）。
429 を受けたら 30 秒から倍々で待って取り直す。

## 表

`rating_events`（立花 API 由来。検証に使うのはこちら）

| 列 | 中身 |
|---|---|
| `feed_date` | **この情報を知れた日**。07:10 配信なので当日の寄り前に使える |
| `pub_label` | 見出しの「（9/9）」＝発表日の月日。前営業日ぶんなので `feed_date` とずれる |
| `code` | 銘柄コード（4 桁。`p_ISL` 由来） |
| `kind` | `rating`（投資判断） / `target`（目標株価） |
| `direction` | `up` / `down` / `new` |
| `news_id` `news_time` | 元のニュース |

`news_days` は取り込んだ配信日（件数 0 の日と未取り込みの日を区別するため）。

`traders_ratings`（トレーダーズ由来。2005 年〜）

| 列 | 中身 |
|---|---|
| `pub_date` | 掲載日。**大引け後に載るので、使うのは翌営業日から** |
| `code` `name` `market` | 銘柄コード・銘柄名・時点の市場区分（東P / 東1） |
| `firm` | シンクタンク（証券会社） |
| `rating` `target` | 原文（`Buy→Hold` / `3600→2600円`） |
| `rating_from` `rating_to` | 判断の前後 |
| `target_from` `target_to` | 目標株価の前後（円）。`-` の行は 0 |
| `direction` | `up` / `down` / `keep` / `new` / `unknown` |

`traders_months` は取り込んだ年月。

`ratings`（グレイル由来）

| 列 | 中身 |
|---|---|
| `pub_date` | 一覧の掲載日（年は取得日から補う） |
| `code` `name` `firm` | 銘柄コード・銘柄名・証券会社 |
| `rating` `target` | 原文（`買い継続` / `9300円→9500円`） |
| `action` | `new` / `up` / `down` / `keep` |
| `rating_from` `rating_to` | 判断の前後 |
| `target_from` `target_to` | 目標株価の前後（円） |
| `first_seen_at` | **初めて見た時刻**（JST）。掲載時刻の代わり |
| `last_seen_at` | 最後に見えた時刻 |

主キーは `(pub_date, code, firm, rating, target)`。
内容が訂正された行は別の行として増える（初出時刻を訂正で塗り替えないため）。

`fetches` は取りに行った記録（`fetched_at` `rows` `new_rows` `status` `elapsed_ms`）。

## 検証に使うとき

**検証の記録はこのリポジトリではなく `~/obsidian-vault/20-research/` に書く**（[CLAUDE.md](../CLAUDE.md)）。
