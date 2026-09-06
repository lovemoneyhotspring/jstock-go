# 本番デプロイ チェックリスト

詳細は README.md の「APIキーの置き場所」「cron で回す」参照。ここは持っていく物と手順の要約だけ。

## 1. サーバーに持っていく物

| 種類 | 場所 | git管理 |
|---|---|---|
| コード | `cmd/` `pkg/` `go.mod` `go.sum`（サーバーで `deploy/build.sh` を叩いて `bin/` を作る） | ✅ |
| 設定 | 運用する戦略の `config/<戦略名>/` 一式 | ✅ |
| 秘密情報 | J-Quants APIキーと立花証券の認証情報（認証ID・秘密鍵 PEM・第二暗証番号）。`.env` または systemd `EnvironmentFile=` | ❌ 別途用意 |
| キャッシュ | `data/`（足・財務・J-Quants アーカイブ） | ❌ 別ホストからコピーしてよい（または再取得） |
| 状態 | `state/`（発注台帳・ログ・バックアップ） | ❌ **このホスト固有。他ホストのファイルで上書き厳禁** |

`config/` と `data/` はコードの外にあるので、**`cmd/` `pkg/` だけ配ると動かない**。

## 2. セットアップ手順

```bash
# 1. コード配置
git clone https://github.com/lovemoneyhotspring/jstock-go.git /home/abobo/jstock-go && cd /home/abobo/jstock-go
# private リポジトリなので認証が必要。HTTPS の場合はパスワード欄に PAT（Personal Access Token）を使う。
# SSH鍵を使うなら代わりに: git clone git@github.com:lovemoneyhotspring/jstock-go.git /home/abobo/jstock-go

# 2. 実行ファイルを作る（Go 1.26 以上。uv も Python も要らない）
deploy/build.sh          # bin/ に wbjp / accum / daytrade / jquants / discord-post ができる

# 3. APIキー（J-Quants）と立花証券の認証情報（2b 節）
# ヘッドレスLinuxにはkeyringが無いので、キーチェーンは使わず
# systemd 経由で環境変数として渡す。
sudo install -d -m 750 -o root -g wbjp /etc/wbjp
sudo install -m 640 -o root -g wbjp /dev/null /etc/wbjp/wbjp.env
sudo vi /etc/wbjp/wbjp.env
# ↑ このファイルに以下を書く（値は自分のAPIキーに差し替え）
#   WBJP_JQUANTS_API_KEY=...   # 日本株の足（J-Quants）。環境で分けない
#   TACHIBANA_PROD_AUTH_ID=... # 立花証券（2b 節の 3 つ）
#   TACHIBANA_PROD_PRIVATE_KEY_FILE=/etc/wbjp/tachibana-prod.pem
#   TACHIBANA_PROD_ORDER_PASSWORD=...
#
# systemdサービス定義（/etc/systemd/system/wbjp.service）に
# EnvironmentFile=/etc/wbjp/wbjp.env を1行書けば、起動時にこのファイルの
# 中身がプロセスの環境変数として読み込まれる（cronで動かす場合は不要、
# 4節のcron行で直接 .env を読ませる）

# 4. cron のリダイレクト先を先に作る（アプリが作るのは起動後のため）
mkdir -p state/logs

# 5. 価格データの初期取り込み（運用する戦略のconfigごとに）
wbjp data sync --config-dir config --days 1500

# 6. dry-runで確認（--live無しは常に発注しない）
WBJP_ENV=prod wbjp run --config-dir config
```

## 2b. 立花証券 e支店（`execution.broker = "tachibana"`。全プロジェクト共通）

発注（スイング・積立・デイトレ）は全て立花証券で出す。次の 3 つが要る。

1. e支店 Web（標準 Web）の［お客様情報］→［ｅ支店・ＡＰＩ利用設定］で **「利用する」** にし、
   自動生成される **認証ID** を控える（「ＤＬ」で `e_api_authid.txt`）
2. 同じ画面で **公開鍵を登録**（自動作成なら表示される秘密鍵を 1 度だけ「ＤＬ」できる。
   手動なら自分で RSA 2048/4096 の鍵対を作り、公開鍵だけ登録）。秘密鍵は PEM で保存する。
   ログイン応答の仮想URLはこの公開鍵で暗号化されて返るので、秘密鍵が無いと何もできない
3. ［設定情報］→［第二暗証番号］で **「暗証番号省略」を無効**にする（API の注文は毎回
   `sSecondPassword` が必須。省略設定のままだとエラー 11029）。交付書面が未読だと仮想URLが発行されない

本番とデモ（`https://demo-kabuka.e-shiten.jp/`）は認証ID・鍵が別管理。`.env`（0600）に:

```bash
TACHIBANA_PROD_AUTH_ID=...                 # 認証ID
TACHIBANA_PROD_PRIVATE_KEY_FILE=/etc/wbjp/tachibana-prod.pem   # 秘密鍵（0600）
TACHIBANA_PROD_ORDER_PASSWORD=...          # 第二暗証番号
TACHIBANA_UAT_AUTH_ID=...                  # デモ環境（WBJP_ENV=uat）
TACHIBANA_UAT_PRIVATE_KEY_FILE=/etc/wbjp/tachibana-uat.pem
TACHIBANA_UAT_ORDER_PASSWORD=...
```

- 手数料コースは Web で**定額手数料コース**を選ぶ（現物は 1 日の約定代金合計 12 万円まで 0 円、
  20 万円まで 176 円…。信用は 0 円で現物とは別計算）。積立（月 2〜2.5 万、4 倍でも 10 万）は
  ほぼ無料の範囲。`preview` の手数料見積りはこのコース前提で、当日の既約定分
  （`sGenbutuBaibaiDaikin`）を足した段階の差分を出す
- 積立（`accum`）と信用デイトレは同じ現金を使う。デイトレの建玉が 9:00〜15:20 に保証金を拘束するので、
  14 時台の積立が見る現物買付可能額はその残り。月の積立額が収まるかを一度確かめる
- その日の通番（`p_no`）と復号した仮想URLは `state/tachibana/session-<env>-<YYYYMMDD>.json`（0600）に
  残し、同じ日は再ログインしない（公式サンプルと同じ）。仮想URLが無効化されたらこのファイルを消す
- 初回は**デモで** `WBJP_ENV=uat daytrade quotes 7203 9984 --config-dir config/daytrade_margin` と
  `daytrade open --config-dir config/daytrade_margin`（dry-run）で疎通と電文を確かめる。
  サーバの外向き IP の許可設定は不要

## 3. `.env` の置き場所

- **既定**: リポジトリ直下（カレントディレクトリ基準）= `/home/abobo/jstock-go/.env`。`chmod 600` 必須（緩いと起動時に警告）
- **絶対パスで指定したい場合**: 環境変数 `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` を渡せば、cronの`cd`忘れがあってもそこを読む
- 中身は秘密でない項目（`WBJP_ENV=prod` 等）のみ。APIキー自体は上記手順3のsystemd `EnvironmentFile=` 経由が推奨（ローカル開発ならキーチェーン。README「APIキーの置き場所」）

## 4. 定期実行（cron）— 20分おき固定、時刻計算はしない

`wbjp run` と `accum run` はどちらも「呼ばれた時点で必要な分だけ判断し、不要なら何もしない」
作りになっている。

- 注文IDは日付・銘柄・数量から決定論的に作られる → 何度呼んでも二重発注しない
- `accum run` は発注してよい時間帯（既定14:00〜15:00 JST）の外では自動でスキップする
- 新しい確定足が無ければ`wbjp run`は前回と同じ判断になるだけ（無害）

なので**市場が開いているか・引けたかを cron 側で計算する必要はない**。固定間隔で叩き続ければ、
時間帯の判定は各コマンドの中で完結する。

開始の分は**互いにずらす**。同時に走ると J-Quants のレート制限に当たりやすく、
同じ銘柄の足を二つのプロセスが同時に書く競合も避けられる。
`sleep` でずらすより cron の分指定の方が、crontab を見ただけでタイミングが分かる。

crontab の内容は [`deploy/crontab.txt`](../deploy/crontab.txt) に置いてある（これが正）。
発注経路の行は立花証券の認証情報を入れるまでコメントアウトしてあり、データ取得・監視・
前夜の候補作成（`daytrade plan`）だけが回る。

crontab には**暗号資産の他ジョブと rcguard が同居している**（合わせて 190 行超）。
`grep -v` で jstock-go の行を落とす形だと、コメント行や `WBJP_HOME=` の定義が取りこぼされて
二重に入る。**ブロックごと差し替える**のが確実:

```bash
# 1. 必ずバックアップを取る
mkdir -p state/backup/crontab
crontab -l > "state/backup/crontab/crontab-$(date +%Y%m%d-%H%M%S).txt"

# 2. jstock-go ブロックの範囲を確かめる（先頭は deploy/crontab.txt の 1 行目、
#    末尾は rcguard の "# >>> rcguard" の直前）
BK=$(ls -t state/backup/crontab/*.txt | head -1)
grep -n '^# wbjp（/home/abobo/jstock-go）の cron' "$BK"   # → 開始行
grep -n '# >>> rcguard' "$BK"                              # → rcguard の開始行

# 3. 前後を残して真ん中を差し替える（下の 62 / 158 は 2. で調べた値に置き換える）
{ sed -n '1,62p' "$BK"; cat deploy/crontab.txt; echo; sed -n '158,$p' "$BK"; } > /tmp/crontab.new

# 4. 他のジョブが残っているか数えてから入れる
for p in gmo_coin binance hyliq bitbank rcguard; do
  printf '%-10s 旧 %2d → 新 %2d\n' "$p" "$(grep -c $p "$BK")" "$(grep -c $p /tmp/crontab.new)"
done
crontab /tmp/crontab.new
crontab -l | grep -cF 'env $JQ_MEM'    # 上限が全行に渡っているか
```

戻すときは `crontab state/backup/crontab/<バックアップ>.txt`。

`daytrade open` は 9:01・9:04・9:07・9:10・9:13 の 5 回呼ぶ（板寄せ直後は気配の約定時刻が前日のままで「古い」と判定されることがあるため、9:00 ちょうどは避ける。窓は 9:15 まで）。再実行は**残りの枚数だけ**建てる——前の回が N 銘柄すべて建てていれば何もせず、途中で落ちていれば（通信エラー・締め切り）N − 建てた数を別の銘柄で埋める。同じ銘柄は重ねて建てない。
`close` は 15:20・15:24・15:28 の 3 回（1 回目で売れていれば 2 回目以降は何もしない。拒否されていれば送り直す）。15:20 の成行はその場の気配で約定し、15:25 以降ならクロージング・オークションで引け値になる。15:40 の `verify` は売りの約定を照会し、売れ残り（ストップ安で板に買いが無い等）があれば持ち越しとして通知する。持ち越しは翌朝の `open`（と引けの `close`）が新規に建てる前に成行で手仕舞い、その拘束資金ぶん当日の件数を減らす（`docs/DAYTRADE.md`「持ち越しの扱い」）。祝日は `open` が「候補なし／気配なし」で終わるだけで無害。

`daytrade snap`（8:30・8:45・8:55・8:59／9:00・9:02・9:05・9:10／15:00・15:19）は板・気配を記録するだけで
**発注しない**ので、発注経路を開ける前から回す（板は過去に遡れない。[OPENING_DATA.md](OPENING_DATA.md)）。
`open` / `close` と**同じ `/tmp/daytrade.lock`** を使う——時価問合のレート制限（4 回/秒）はプロセス内なので、
同時に走ると発注側が弾かれうる。ロックを取れなければ `snap` は何もせず終わる（発注が優先）。`snap` 自身は
`book.max_run_seconds`（50 秒）で切れるので、9:00 の `snap` が 9:01 の `open` を長く塞ぐことはない。

### 締め切りとロック（発注が消えないために）

発注経路の 1 回の実行には**締め切り**がある。開始から `execution.max_run_seconds`（150 秒）か、
時間帯（`entry_window` / `exit_window`）の終わりの早い方。過ぎたら新しい電文は送らず、送信中のものは
打ち切る。立花への電文 1 本の待ちは発注・照会 30 秒、時価問合 10 秒で、締め切りが近ければそこまで。
これが無いと、ブローカーが遅い日に 9:01 の 1 回がロックを握ったまま 9:15 を越え、以降の回が全部消える。

ロックは `deploy/with-lock.sh` で取る。`flock -n` は取れないとき**無言で exit 1** するので、
「前の回が長引いて 9:04 が消えた」と「9:04 が走って失敗した」を後から区別できない。
`with-lock.sh` は見送りを `[lock_busy]` としてログに残す。`open` / `close` は 30 秒までロックを待つ。

照会の電文（余力・建玉・注文一覧・時価）は通信エラーで 1 度だけ送り直す。新規注文は送り直さない
（届いていた場合に二重発注になる。送信結果不明は台帳に `PENDING` で残り、次の回が当日の注文一覧で判定する）。
立花への電文は 1 本 1 行で `state/logs/daytrade-prod.jsonl` に残る（`code` = `broker.request`。何を送って、
何ミリ秒待って、何が返ったか。[LOGGING.md](LOGGING.md)）。
`deploy/crontab.txt` では**コメントアウトしてある**——立花証券の資格情報が無い間は接続に失敗して
終了コード 1 になり、1 日 10 回ぶんのログが溜まるだけだからだ。`.env` に `TACHIBANA_*` を入れて
`wbjp credentials check` が通ったら、**発注経路の実機検証（`BROKER_VERIFY.md`）を待たずに開けてよい**。

### 日次レポート（Discord）

レポートの行（`deploy/report.sh`）だけは他と前提が違う。回す前に 2 つ要る。

1. **通知先**。Discord へは **Bot の REST API** で送る（Incoming Webhook は使わない。
   Webhook は通常のテキストチャンネルでスレッドを作れず、投稿が平積みになるため）。
   Developer Portal で Bot を作り、サーバーに「チャンネルを見る」「メッセージを送信」
   「公開スレッドの作成」「スレッドでメッセージを送信」の権限で招待して、`.env` に
   `WBJP_DISCORD_BOT_TOKEN=…` と送り先のチャンネル ID を入れる。

   | 変数 | 送るもの |
   |---|---|
   | `WBJP_ALERT_CHANNEL_ID` | 異常（`jquants check --notify`、`daytrade close` の失敗など） |
   | `WBJP_REPORT_CHANNEL_ID` | 21:00 の日次レポート。無ければ `WBJP_ALERT_CHANNEL_ID` に流れる |

   送るたびに新しいスレッドを作り、本文はその中に入る（1 通知 1 スレッド）。本文が
   2000 字を超えるときは分割して同じスレッドに連投する。チャンネル ID は Discord の
   設定で開発者モードを有効にすると、チャンネルの右クリックからコピーできる。

   `WBJP_MENTION_USER_ID` にユーザー ID を入れると、**すべての投稿**の 1 通目に
   `<@ID>` が付いて通知が飛ぶ（レポートは毎日見るものなので既定で付ける想定）。
   本文に `@名前` と書いても Discord は通知しない——ID でないと届かない。
   分割された 2 通目以降には付けない（1 投稿につき通知は 1 回）。

   送ったものは **`state/notify/<日付>.jsonl` に 45 日ぶん控えが残る**。送れなかった
   ものも理由付きで残るので、「昨日の異常通知は何だった？」に Discord を開かずに
   答えられる。45 日なのは、毎月 1 日の月次レポートが前月まるごとを読み返せるようにするため。
2. **Claude Code の認証**。cron は `claude` を非対話で回すので、その cron を持つ
   ユーザーで一度 `claude` にログインしておく（認証は `~/.claude` に入る）。

```bash
# 送らずに中身だけ確認する
DRY_RUN=1 /home/abobo/jstock-go/deploy/report.sh daily

# 配達だけ試す（Discord に 1 通届けば経路は通っている）
echo 'テスト' | bin/discord-post
```

### レポートの種類

| 種類 | いつ | 対象 | 本文の残り先 | 保持 |
|---|---|---|---|---|
| 日次 | 平日 21:00 | その日 | `state/reports/daily-<日付>.md` | 45 日 |
| 週次 | 金 21:30 | その週の月〜金 | `state/reports/weekly-<開始日>.md` | 180 日 |
| 月次 | 毎月 1 日 22:00 | 前月まるごと | `state/reports/monthly-<YYYY-MM>.md` | 消さない |

日次は `.claude/agents/daily-report.md`、週次・月次は `.claude/agents/periodic-report.md`
が「何をどの順で読むか」を決める。週次・月次は**日次レポートの .md に依存しない**
——追記専用の履歴（Parquet）とダイジェストを期間で読むので、日次の保持期間より
長い範囲でも振り返れる。仕組みは [FEEDBACK.md](FEEDBACK.md)「4. レポート（Discord）」。

### cron を入れる前の検証

cron 専用の「実行せずに全部検証する」コマンドは無い。次の 3 段で確かめる:

```bash
# 1. 構文チェック。cronie 系（RHEL / 新しめの Ubuntu）なら投入せずに検証できる
crontab -T /path/to/crontab.txt
#    -T が無い環境（Debian 系・macOS）でも、crontab は投入時に構文を検証して
#    不正なら "errors in crontab file" で丸ごと拒否する（部分投入はされない）
crontab /path/to/crontab.txt && crontab -l

# 2. リダイレクト先を先に作る。>> state/logs/… の評価はプロセス起動より前なので、
#    ディレクトリが無いと本体が一度も走らずに失敗する（アプリが作るのは起動後）
mkdir -p /home/abobo/jstock-go/state/logs

# 3. コマンド部分の検証は 1 回手で流すしかない。cron と同じ最小環境を再現して実行する
#    （--live は外す。発注以外のデータ取得・判断・記録はすべて動く）
cd /home/abobo/jstock-go && env -i HOME=$HOME PATH=/usr/bin:/bin WBJP_ENV=prod \
  bin/wbjp run --config-dir config
cd /home/abobo/jstock-go && env -i HOME=$HOME PATH=/usr/bin:/bin WBJP_ENV=prod \
  bin/jquants sync --dry-run
```

構文チェックが見てくれるのは「5 つの時刻フィールド＋コマンドの形」だけで、
パスの誤り・権限・環境変数はコマンドを実際に流さないと分からない。
`env -i` で流すのは、対話シェルの PATH や .zshrc に助けられて「手では動くが
cron では動かない」を潰すため。ほかに cron 固有の罠は `%` （cron では改行の
意味。コマンドに書くなら `\%`）と、`MAILTO` 未設定時にエラーメールが捨てられる
こと（このリポジトリは stderr をログへリダイレクトしているので影響しない）。

- **1 回きりの cron を作らない。** 失敗すると次の機会（翌日・翌月）までバックアップや
  取り込みの無い状態が続くため、どのジョブも「何度叩いても同じ（冪等）」に作り、
  頻度を上げて失敗を次の実行で自動回復させる。失敗自体は Discord（`WBJP_ALERT_CHANNEL_ID`）に通知される
  - `accum backup` は毎時（37 分）。同日分は上書きなので世代は 1 日 1 つのまま。
    1 回失敗しても 1 時間後に取り直し、失敗が続けば毎時通知が来る
  - 月次の `jquants backfill` は 2〜5 日の 4 回。成功していれば 2 回目以降は
    更新された一括ファイル（過誤訂正、月末数日の取り漏れ）だけを取り直す
- 平日 20:00 の `jquants check --notify` は欠けの監視。当日ぶん（16:30〜18:00 公開）が
  取れていなければ Discord（`WBJP_ALERT_CHANNEL_ID`）に通知する（未設定ならログのみ）

- `accum backup` は `state/` の全 SQLite（積立台帳 `accum-prod.db` とスイング売買の記録
  `wbjp-prod.db`）を `state/backup/<名前>-YYYYMMDD.db` に複製する（各 30 世代、SQLite の
  オンラインバックアップなので実行中でも一貫する）。
  台帳は「今月いくら発注済みか」の唯一の記録で失うと当月を買い直すので、`state/backup/` は
  別ディスクやオブジェクトストレージへ同期しておく。`accum run` は起動時にブローカーの当月の
  約定と台帳を突き合わせ、台帳に無い約定があれば発注を止めて通知する
- 選定の履歴 `state/daytrade/history/` と `state/wbjp/history/`（追記専用の Parquet、
  `docs/DAYTRADE.md`「履歴」）は `accum backup` の対象外。ファイルは増えるだけで書き換わらないので、
  `state/backup/` と一緒に `rsync -a`（`--delete` なし）で別ホストへ同期する
- 平日 20:20 の `daytrade evaluate` は、朝の候補（選んだ銘柄も次点も）に当日の日足を当てて
  `state/daytrade/history/evaluation/` に残す。日足が取り込まれる前に走れば何もせず終わる
  （翌日の 20:20 に `--date` 無しでは前日を拾わないので、抜けた日は手で `--date` を付けて回す）。
  選定の妥当性は `daytrade review` で見る（`docs/DAYTRADE.md`）
- `accum run --live` は発注時間帯（既定 14:00〜15:00 JST、土日除く）の外では足の同期も
  ブローカー接続もせずに終了する。`--live` 無しの dry-run は確認用なので、いつでも判断まで見せる

- `flock -n` は前回の実行がまだ終わっていない場合に二重起動しないためのロック（万一20分より処理が長引いた時の保険。ファイルは初回に自動作成される）。デイトレの行は `deploy/with-lock.sh` 経由で、ロックを取れずに見送った回もログに残る
- ログを見て「発注時間帯の外」「新規足なし」ばかりなら正常（判断はしているが何もしていないだけ）

## 4-b. 発注経路を開ける前に

注文照会（`CLMOrderList`）・信用建玉・信用の発注は**実機で未検証**。実装は入っているが、
項目名が資料からの推定を含む。応答の形が違えば必ず失敗する作りにしてあるので壊れはしないが、
**確認が済むまで発注は動かない**。手順は [BROKER_VERIFY.md](BROKER_VERIFY.md)。

## 5. 緊急停止

`config/<戦略名>/settings.toml` の `risk.kill_switch = true` で即停止（cronは消さなくてよい）。

## 6. 更新（GitHub から取り込む）

```bash
cd /home/abobo/jstock-go
flock /tmp/accum-run.lock git pull --ff-only   # 走行中の accum run と重ならないようにロックを取る
make ci                                        # 任意: build + vet + staticcheck + test（push 時の CI と同じ）
flock /tmp/accum-run.lock deploy/build.sh      # 実行ファイルを作り直す（走行中と重ならないように）
```

- **cron の再起動は不要**。`wbjp run` / `accum run` は 20 分ごとに新しいプロセスで起動するので、`build.sh` を回した次の実行から新コードになる
- **`build.sh` を忘れると古い実行ファイルのまま動く**。`git pull` だけでは反映されない（Go は実行ファイルに固める）
- `data/`（台帳 DB・足・ログ）は git 管理外なので pull で消えない
- `config/` は git 管理下。サーバー側で `accum.toml` を直接編集すると pull が衝突する。
  設定変更は **ローカルで commit → push → サーバーで pull** の一方向に揃える
- `--ff-only` が失敗する（履歴が書き換えられた等）ときは、サーバーに手元の変更が無いことを
  `git status` で確かめてから `git fetch origin && git reset --hard origin/main`
