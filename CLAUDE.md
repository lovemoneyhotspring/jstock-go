# 応答言語

ユーザへのメッセージ（会話での説明・要約・確認事項など）はすべて日本語で書く。
コード・コミットメッセージ・識別子・ファイル内のコメントは既存の慣習に従う（このリポジトリでは日本語のコメントが標準）。

# 記録の置き場

**検証・運用・判断の記録は Obsidian の vault（`~/obsidian-vault`）に書く。このリポジトリには書かない。**
このリポジトリに残すのはコードと、コードの使い方のドキュメント（`docs/*.md`）だけ。

| 書くもの | 場所 |
|---|---|
| 検証（バックテスト・要因の検定）の記録 | `~/obsidian-vault/20-research/`（雛形: `90-templates/検証ノート.md`）。**数字は同じ commit で `20-research/結果.csv` にも足す** |
| 日々の運用の記録 | `~/obsidian-vault/10-journal/YYYY-MM-DD.md` |
| 週次・月次レポート | `~/obsidian-vault/50-reports/`（`deploy/report.sh` が自動で入れる） |
| 検証から抽出した原則 | `~/obsidian-vault/40-insights/` |
| このシステムの常設ノート | `~/obsidian-vault/30-projects/`（`jstock-go` `daytrade` `wbjp` `accum` `jquants`） |

**読むときは索引から入る。** 全文を開くのは必要と分かってから。

| 知りたいこと | まず読む | 大きさ |
|---|---|---|
| 検証の結論 | `~/obsidian-vault/20-research/結論一覧.md` | 60 KB（全文は 1.1 MB） |
| 検証の数字（DD・Sharpe・CAGR で絞る） | `bin/jquants query "… read_csv('~/obsidian-vault/20-research/結果.csv') …"` | 約 1,800 行 |
| このリポジトリの仕様 | `docs/README.md` | 1 KB（全文は 300 KB） |

書き方の規約は `~/obsidian-vault/CLAUDE.md`。vault は独立した git リポジトリ（private）なので、
**書いたら vault 側で commit して push まで済ませる**。頼まれるのを待たない。
push していない記録は PC 側の Obsidian から見えず、存在しないのと同じ。

2026-09-07 より前の検証は `docs/research/` にあった。git の履歴に残っている。

**検証のスクリプトは `test/` に置いて commit する。`/tmp` に書かない。**
`/tmp` は消える。2026-09-20 に候補表の再現（`/tmp/dt_rebuild.py`）や walk-forward が丸ごと消えていて、
ノートの手順どおりに再現できなかった。ノートの「手順」に書くコマンドは `test/*.py` を指すこと。
中間出力は `test/out/`（git の管理外）。候補表は `test/dt_candidates.py`、walk-forward の土台は `test/dt_wf_target.py`。

# 文脈を膨らませない

読んだものは会話が続く限り毎ターン送り直される。利用上限を早く使い切る主因はこれ。

- ファイルを丸ごと `cat` しない。`grep -n` で場所を絞り、`sed -n 'a,bp'` か Read の offset/limit で要る範囲だけ読む
- 複数ファイルにまたがる調査・広い探索は Explore サブエージェントに任せ、結論だけ受け取る
- ログやコマンド出力は `tail` / `grep` / `wc` で絞ってから出す
- 話題が変わったらユーザに `/clear` を勧める
