# 応答言語

ユーザへのメッセージ（会話での説明・要約・確認事項など）はすべて日本語で書く。
コード・コミットメッセージ・識別子・ファイル内のコメントは既存の慣習に従う（このリポジトリでは日本語のコメントが標準）。

# 記録の置き場

**検証・運用・判断の記録は Obsidian の vault（`~/obsidian-vault`）に書く。このリポジトリには書かない。**
このリポジトリに残すのはコードと、コードの使い方のドキュメント（`docs/*.md`）だけ。

| 書くもの | 場所 |
|---|---|
| 検証（バックテスト・要因の検定）の記録 | `~/obsidian-vault/20-research/`（雛形: `90-templates/検証ノート.md`） |
| 日々の運用の記録 | `~/obsidian-vault/10-journal/YYYY-MM-DD.md` |
| 週次・月次レポート | `~/obsidian-vault/50-reports/`（`deploy/report.sh` が自動で入れる） |
| 検証から抽出した原則 | `~/obsidian-vault/40-insights/` |
| このシステムの常設ノート | `~/obsidian-vault/30-projects/`（`jstock-go` `daytrade` `wbjp` `accum` `jquants`） |

**読むときは索引から入る。** 全文を開くのは必要と分かってから。

| 知りたいこと | まず読む | 大きさ |
|---|---|---|
| 検証の結論 | `~/obsidian-vault/20-research/結論一覧.md` | 3 KB（全文は 225 KB） |
| このリポジトリの仕様 | `docs/README.md` | 1 KB（全文は 155 KB） |

書き方の規約は `~/obsidian-vault/CLAUDE.md`。vault は独立した git リポジトリ（private）なので、
**書いたら vault 側で commit して push まで済ませる**。頼まれるのを待たない。
push していない記録は PC 側の Obsidian から見えず、存在しないのと同じ。

2026-09-07 より前の検証は `docs/research/` にあった。git の履歴に残っている。
