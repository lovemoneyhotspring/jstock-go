# docs の索引

**開く前にここで当たりを付ける。** 合計 300 KB あるので、全部読むと文脈を使い切る。

| 知りたいこと | ファイル | 大きさ |
|---|---|---|
| デイトレの規則・1 日の流れ・設定・危険信号 | [DAYTRADE.md](DAYTRADE.md) | 85 KB |
| J-Quants の何をどう溜めているか（端点・メモリ・分足・ティック） | [JQUANTS_ARCHIVE.md](JQUANTS_ARCHIVE.md) | 45 KB |
| 本番への持っていき方・cron・緊急停止・更新手順 | [DEPLOY.md](DEPLOY.md) | 41 KB |
| ログの項目・`code` の一覧・読み方 | [LOGGING.md](LOGGING.md) | 35 KB |
| 板・分足を集める理由と段階 | [OPENING_DATA.md](OPENING_DATA.md) | 19 KB |
| 改善ループ（ダイジェスト → evaluate/review → レポート）と自動化の線引き | [FEEDBACK.md](FEEDBACK.md) | 12 KB |
| 立花証券 API の未検証の電文と UAT の確認順 | [BROKER_VERIFY.md](BROKER_VERIFY.md) | 47 KB |
| 証券会社のレーティングをいつ・どう溜めているか | [RATE.md](RATE.md) | 8 KB |
| 立花証券のニュース電文を溜める仕組みと、何が流れているか | [NEWS.md](NEWS.md) | 8 KB |

節の見出しは各ファイルの先頭で `grep '^## '` すれば分かる。**必要な節だけ `sed -n` で読む。**

## 検証の記録はここには無い

Obsidian の vault に移した（2026-09-07）。

- **結論だけなら `~/obsidian-vault/20-research/結論一覧.md`（60 KB。検証ごとの一行結論）**
- 全文は `~/obsidian-vault/20-research/`
- 経緯は [research/README.md](research/README.md)
