# 応答言語

ユーザへのメッセージ（会話での説明・要約・確認事項など）はすべて日本語で書く。
コード・コミットメッセージ・識別子・ファイル内のコメントは既存の慣習に従う（このリポジトリでは日本語のコメントが標準）。

# 記録の置き場

**検証・運用・判断の記録は Obsidian の vault（`~/obsidian-vault`）に書く。このリポジトリには書かない。**
このリポジトリに残すのはコードと、コードの使い方のドキュメント（`docs/*.md`）だけ。

| 書くもの | 場所 |
|---|---|
| 検証（バックテスト・要因の検定）の記録 | `~/obsidian-vault/20-research/`（テンプレート: `90-templates/検証ノート.md`） |
| 日々の運用の記録 | `~/obsidian-vault/10-journal/YYYY-MM-DD.md` |
| 週次・月次レポート | `~/obsidian-vault/50-reports/` |
| 検証から抽出した原則 | `~/obsidian-vault/40-insights/` |

vault 側の書き方の規約は `~/obsidian-vault/CLAUDE.md` にある。vault は独立した git リポジトリ
（private）なので、書いたら vault 側でも commit する。

2026-09-07 より前の検証は `docs/research/` にあった。git の履歴に残っている。
