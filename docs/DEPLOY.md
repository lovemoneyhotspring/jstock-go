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

# 2. 実行ファイルを作る（Go 1.27 以上。uv も Python も要らない）
deploy/build.sh          # bin/ に wbjp / accum / daytrade / jquants / discord-post / rate / news ができる

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
   手動なら自分で RSA 2048/4096 の鍵対を作り、公開鍵だけ登録）。自動作成で落ちてくるのは
   **DER**（`e_api_private_key.der`、バイナリ）で、PEM に変換しなくてもそのまま読める
   （`parseRSAPrivateKey` が PEM / DER の両方を試す）。`.gitignore` は `*.pem` `*.key` `*.der` を弾く。
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
- **絶対パスで指定したい場合**: 環境変数 `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` を渡せば、cronの`cd`忘れがあってもそこを読む。**相対パスの設定（`WBJP_STATE_DIR=state` など）は読み込んだ `.env` のある場所から解く**（作業ディレクトリからではない）ので、`.env` をリポジトリの外に置くなら `WBJP_STATE_DIR` / `WBJP_DATA_DIR` / `WBJP_CONFIG_DIR` は絶対パスで書く。本番のブローカーは state ディレクトリが無い場所からは作れない（別のセッションで新規ログインして本番のセッションを切らないため）
- 中身は秘密でない項目（`WBJP_ENV=prod` 等）のみ。APIキー自体は上記手順3のsystemd `EnvironmentFile=` 経由が推奨（ローカル開発ならキーチェーン。README「APIキーの置き場所」）

## 4. 定期実行（cron）— 20分おき固定、時刻計算はしない

`wbjp run` と `accum run` はどちらも「呼ばれた時点で必要な分だけ判断し、不要なら何もしない」
作りになっている。

- 注文IDは日付・銘柄・数量から決定論的に作られる → 何度呼んでも二重発注しない
- `accum run` は発注してよい時間帯（既定14:00〜15:00 JST）の外では自動でスキップする
- 新しい確定足が無ければ`wbjp run`は前回と同じ判断になるだけ（無害）
- `wbjp run` は休場日（東証カレンダー）なら判断も発注もせずに終わる。カレンダーが読めなければ
  発注する回は止まる。前営業日の足が無い（古い・読めない）銘柄は、その回は売りも買いも出さない
  （`wbjp.bars_unusable`）。**保有中の銘柄なら損切りも止まる**（古い足で損切りを決めない代わりに、
  下げても売らない）。足の取り込みが止まったまま気づかないのを避けるため、保有中の銘柄の足が古い回は
  ダイジェストに加えて Discord にも通知する
- `wbjp run --live` は、建玉の照会がエラーなしで 0 件なのに台帳では保有中のはず（保存済みのストップ・
  前の回以降に約定した買い）の銘柄があると止まる（`wbjp.positions_empty`）。照会の不調を信じて
  ストップを消し、保有中の銘柄を買い直さないため。手で全部売った後など口座が本当に空なら、
  確かめたうえで `--accept-flat` を付けて 1 回走らせる

なので**市場が開いているか・引けたかを cron 側で計算する必要はない**。固定間隔で叩き続ければ、
時間帯の判定は各コマンドの中で完結する。

ただし `wbjp run` の行は、デイトレの発注（9:00 前後の open・snap、:x6 の guard、protect、15:20〜の close）と
立花の API を取り合わないよう、**平日の場中 2 回（9:31・13:31）**に絞ってある（2026-09-24 のレビュー W7。
以前の `*/20 * * * *` は土日・夜間も含めて 1 日 72 回で、9:00:00・15:20:00 にデイトレと重なっていた）。

開始の分は**互いにずらす**。同時に走ると J-Quants のレート制限に当たりやすく、
同じ銘柄の足を二つのプロセスが同時に書く競合も避けられる。
`sleep` でずらすより cron の分指定の方が、crontab を見ただけでタイミングが分かる。

crontab の内容は [`deploy/crontab.txt`](../deploy/crontab.txt) に置いてある（これが正）。
デイトレ（`daytrade`）は 2026-09-15 から本番（`--live`）で回っている。スイング（`wbjp run`）と
積立（`accum run`）の発注経路はまだ開けておらず、その行はコメントアウトしてある
（データ取得・監視・評価は回る）。`state/` のバックアップ（`accum backup`）は発注しないので、
2026-09-24 に発注の行から切り離して有効にした。

crontab には**暗号資産の他ジョブと rcguard が同居している**（合わせて 440 行超）。
`crontab deploy/crontab.txt` で丸ごと入れると他のジョブが消える。**jstock-go のブロックだけ差し替える**
スクリプトを使う（ブロックの先頭は `# wbjp（` の行、末尾は `# >>> rcguard` の直前。目印が見つからなければ何もしない）:

```bash
deploy/install-crontab.sh --dry-run   # 入れたあとの crontab との差分だけ見る
deploy/install-crontab.sh             # バックアップを取り、構文を検査し、ブロックの外が 1 行も変わらないことを確かめて差し替える
```

バックアップは `state/backup/crontab/`、差し替えた全体の控えは `state/crontab.good`
（下の「cron とは別系統の実行役」が、crontab が消えたときにここから戻す）。戻すときは
`crontab state/backup/crontab/<バックアップ>.txt`。控えは日曜 4:30 の `deploy/prune-state.sh` が、90 日を過ぎて新しい方から 20 世代に入らないものを消す（下の「置き場の掃除」）。

### cron とは別系統の実行役（人が気づく前提にしない）

運用者は別の作業をしていて、通知に気づかず端末も触れない。cron が走らなかったときに人手なしで安全側に
倒れるよう、**systemd のユーザタイマー**（cron と別のデーモン）と**ブローカーに置く保険の注文**を持つ。

| 起きること | 効く手段 |
|---|---|
| cron が止まった・crontab が消えた・cron の行が壊れた | `jstock-close-net`（平日 15:22・15:26）が、当日の close の成功記録（ダイジェスト）が無ければ代わりに `close` を走らせる |
| crontab から jstock-go のブロックが消えた（上書き・空） | `jstock-guard`（平日 8:42・15:12）が `state/crontab.good` から戻す |
| 寄る前の点検が落ちた（設定を読めない・plan が無い） | `jstock-guard`（8:42）が直す: 設定は `deploy/build.sh` で作り直し（作業ツリーが**コミット済みの main** のときだけ）→ まだ動かなければ `deploy/rollback-bin.sh` で 1 世代前へ。plan は `daytrade plan --if-missing`。台帳・ディスクは通知のみ |
| **マシン停止・ネット断** | ローカルの仕組みでは救えない。**ブローカーに置いた保険の引け注文**（`execution.protect_exit`。`daytrade protect`）が引けで手仕舞う。2026-09-20 から有効（実機で未検証のまま本番で検証中。`docs/BROKER_VERIFY.md`「引けの保険注文」）。戻すなら `protect_exit = false`。**保険は後場（12:31〜）だけ**なので、前場（9:00〜12:30）に止まると救えない（前場に置いた「引け」は前引けで約定する） |

```bash
deploy/install-systemd.sh           # 入れて有効にする（sudo 不要。linger が有効なこと）
deploy/install-systemd.sh --remove  # 外す
systemctl --user list-timers 'jstock-*'
deploy/tests/guards_test.sh         # スクリプトの試験（スタブの隔離環境。本物には触れない）
deploy/tests/wait_rollback_test.sh  # wait-until.sh・rollback-bin.sh と、morning-check.sh が時刻待ちの見送りを拾うことの試験
deploy/tests/with_lock_test.sh      # with-lock.sh の終了コード（打ち切り 124・SIGKILL 137・見送り 75）とログの試験
deploy/tests/prune_state_test.sh    # prune-state.sh（置き場の掃除）の保持の決まりと --dry-run の試験
```

- ログは `state/logs/systemd-guard.log`。**通知は「自動で対応した／できなかった」の結果**を 1 通（何も問題が
  無ければ出さない）
- crontab を**意図して止める**ときは `state/crontab.paused` を作る（あると crontab の復元を飛ばす）。
  jstock-go の行だけをコメントアウトして止めた場合（`# wbjp（` の目印が残っている）は、戻さず通知だけ。
  ふつうの止め方は `execution.kill_switch = true`（cron は消さなくてよい）
- 8:40 の cron の点検（`daytrade preflight`）と 8:42 の `jstock-guard` は、問題のある朝は通知が 2 通になる
  （点検結果と復旧結果）。cron が止まっていても 8:42 の側は動く
- 実行ファイルを作り直すたびに、いまの一式が `bin/.prev` に 1 世代残る（1 つでも中身が変わったとき。ハードリンク）

`daytrade open` は寄る前の回（8:59:50.0。下記）と 9:00:01.2・9:00:25・9:01:03・9:03:03・9:06:03・9:09:03・9:13:03 の計 8 回呼ぶ（窓は 8:59〜9:15）。9:00 の回は crontab の `DT_OPEN_AT` ちょうどに始める（8:59 の行から起こして `deploy/wait-until.sh` で待つので、cron の起動の揺れが乗らない。戻すなら `09:00:04.0`）。**時価問合に始値が入るのが何秒からかを刻んで確かめている途中**で、朝ごとに `test/dt_open_quote_timing.py` で「寄った」の割合（基準 99.0%）を見て早めるか遅くするかを決める。9:00 ちょうどから始めるのは、寄った銘柄は寄付の直後に上がり、成行は早いほど安く買えるため（vault 2026-09-jp-daytrade-ml-entry-timing）。9:00:25・9:01:03 は初回がすぐ落ちた朝の早い再試行（9:00:25 は 9:00 の snap の後に回る）——寄った銘柄は遅れるほど高く買い、9:05 に建てると優位がほぼ消えるので、9:03 まで待たせない。初回が長引いてロックを握っていれば 30 秒待って見送り、9:03 の回が拾う。9:00 の snap は open の後に回るよう 30 秒までロックを待つ。再実行は**残りの枚数だけ**建てる——前の回が N 銘柄すべて建てていれば何もせず、途中で落ちていれば（通信エラー・締め切り）N − 建てた数を別の銘柄で埋める。同じ銘柄は重ねて建てない。
**寄る前の回の開始は crontab の `DT_PREOPEN_AT`（8:59:50.0。`deploy/wait-until.sh` で cron の揺れなしに始める。戻すなら `08:59:45.0` と `WITH_LOCK_TIMEOUT=20`）。** 遅いほど気配は始値に近いが、9:00:00 までに注文が届かなければ意味が無いので、朝ごとに `test/dt_preopen_timing.py` の `margin_s`（最後の注文の受理から 9:00:00 まで）を見て刻んで遅らせている。8:59:53 より遅くするなら 8:59:55 の `snap` を先に動かす（同じロック）。`DT_PREOPEN_AT` + `WITH_LOCK_TIMEOUT`（15 秒）は 9:00:05 以上に保つ（9:00:00 ちょうどに切ると最後の寄成の送信中に当たる）。

**寄る前の回は寄成でロングを出す**（`execution.preopen_legs = "long"`。2026-09-19〜）。寄成の仕組み・9:00:00 の締め切り・
建たなかった分を 9:00 以降の回が「残りの枚数」で埋めること・`sCondition ≠ 0` が実機未検証なことは `docs/DAYTRADE.md`
「寄る前に出す」。実測 4〜9 秒で終わるので `WITH_LOCK_TIMEOUT` は 15 秒で足りる。

この回にロックを明け渡すため、**8:59 台後半の `snap` は 8:59:35（`--slot 085935`、`--max-run 8` / 打ち切り 10 秒）と
8:59:55（`--slot 085955`、`--max-run 2` / 打ち切り 4 秒）の 2 回に割ってある**。どちらもロックを待たず（待ち 0 秒）、`open` が
握っていれば何もせず終わる——だから `morning-check.sh` の必須スロットには入れていない。
**`--max-run` は必ず打ち切り（`WITH_LOCK_TIMEOUT`）より手前に置く**。
`--max-run` で切り上げた回は取れたバッチを記録して正常に終わるが、打ち切りの TERM が先に当たると何も
記録せず `[timeout]` が残り、固まった回と見分けられない。
**8:59:00 の `snap` は `--max-run 20` / 打ち切り 25 秒**（35 / 40 だと固まった朝に 8:59:40 まで握り、
8:59:35 の回が黙って落ちて `open` の余裕も削る）。
寄る前の回が見た気配そのものは `open` が `history/quotes` に残すので、始値との誤差はそちらで測る。

`close` は 15:20・15:24・15:28 の 3 回（1 回目で売れていれば 2 回目以降は何もしない。拒否されていれば送り直す）。15:20 の成行はその場の気配で約定し、15:25 以降ならクロージング・オークションで引け値になる。15:40 の `verify` は売りの約定を照会し、売れ残り（ストップ安で板に買いが無い等）があれば持ち越しとして通知する。持ち越しは翌朝の `open`（と引けの `close`）が新規に建てる前に成行で手仕舞い、その拘束資金ぶん当日の件数を減らす（`docs/DAYTRADE.md`「持ち越しの扱い」）。祝日は `open` が「候補なし／気配なし」で終わるだけで無害。

`daytrade snap`（8:30・8:45・8:55・8:57・8:59:00・8:59:35・8:59:55／9:00:05・9:02・9:05・9:11／15:00・15:19）は板・気配を記録するだけで
**発注しない**ので、発注経路を開ける前から回す（板は過去に遡れない。[OPENING_DATA.md](OPENING_DATA.md)）。
`open` / `close` と**同じ `/tmp/daytrade.lock`** を使う——時価問合のレート制限（8 回/秒）はプロセス内なので、
同時に走ると発注側が弾かれうる。ロックを取れなければ `snap` は何もせず終わる（発注が優先）。`snap` 自身は
`book.max_run_seconds`（50 秒）で切れる。9:00 の `snap` は `--max-run 15`（打ち切り 18 秒）なので、9:00:25 以降の `open` を長く塞ぐことはない。

点検の cron は 2 本。8:40 の `daytrade preflight` は寄る前に設定・今日の plan・台帳・ディスクを読み、問題があれば通知する
（「6. 更新」）。9:22 の `deploy/morning-check.sh` は `snap` の必須スロットと `open` がその朝ちゃんと回ったか（回数・
ロック見送り・時刻待ちの見送り）を点検して Discord に 1 通投げる——判断も修復もしない。手で試すときは必ず
`NO_POST=1`（`QUIET=1` は問題が 0 件のときだけ黙る）。

### 締め切りとロック（発注が消えないために）

発注経路の 1 回の実行には**締め切り**がある。開始から `execution.max_run_seconds`（150 秒）か、
時間帯（`entry_window` / `exit_window`）の終わりの早い方。過ぎたら新しい電文は送らず、送信中のものは
打ち切る。立花への電文 1 本の待ちは発注・照会 30 秒、時価問合 10 秒で、締め切りが近ければそこまで。
これが無いと、ブローカーが遅い日に 9:00 の 1 回がロックを握ったまま 9:15 を越え、以降の回が全部消える。

ロックは `deploy/with-lock.sh` で取る。`flock -n` は取れないとき**無言で exit 1** するので、
「前の回が長引いて 9:06 が消えた」と「9:06 が走って失敗した」を後から区別できない。
`with-lock.sh` は見送りを `[lock_busy]` としてログに残す。`open` / `close` は 30 秒までロックを待つ。
見送りの判定は `flock -E 251` の番兵で行い、コマンド自身が 251 を返したら 250 に写す——
コマンドの終了コードと混ざると、走って失敗した回まで `[lock_busy]` と記録され、
`morning-check.sh` の「ロック見送り」の数が狂う。見送ったときの終了コードは従来どおり 75。

`deploy/wait-until.sh`（`DT_OPEN_AT` / `DT_PREOPEN_AT` の 2 行）は `with-lock.sh` より前で止まりうる——時刻が空・
読めない・120 秒より先なら、後ろの `open` を起こさずに exit 1 する（いつ走るか分からない回は走らせない）。
そのままだと通知もログも残らないので、crontab の行で `WAIT_UNTIL_LOG=state/logs/daytrade-open.log` を渡し、
見送りを `[error] [wait_skip]` として `open` と同じログに 1 行残す。`morning-check.sh` はこれを
「時刻待ちの見送り」として数え、1 件でもあれば ❌ にする（構造化ログには回そのものが現れないので、
起動の回数からは分からない）。

`with-lock.sh` はコマンドに時間の上限も掛ける（`WITH_LOCK_TIMEOUT` 秒。既定 170、0 で無効）。
コマンドが固まるとロックを握ったままになり、`open` が固まれば `guard` と `close` まで見送られるため。
ふだんの実行は長くても 10 秒。過ぎたら TERM、さらに 10 秒（`WITH_LOCK_KILL_AFTER`。8:59:35・8:59:55 の `snap` は 2 秒）で KILL し、`[error] [timeout]` をログに残して 124 で終わる。
上限より前に SIGKILL で終わった回（メモリ不足の OOM killer など）は打ち切りと分けて `[error] [killed]` を残し、137 で終わる
（どちらも timeout(1) の終了コードは 137 になりうるので、上限まで走ったかどうかで見分ける）。`[killed]` を見たら
`journalctl -k | grep -i oom` で確かめる。

**既定（170 + 10）のままでよいのは、TERM と KILL が 8:59:50 の寄成より手前に着く行だけ。** 8:30・8:45・8:55 の `snap` は
最悪でも 08:57:51 なので据え置き、8:57 の回だけは別行に出して 60 + 5 に詰めてある（既定だと KILL が 09:00:01 に当たり、
ロック待ち 5 秒の寄成を `[lock_busy]` で弾く）。8:59 台に行を足すときも同じ計算をすること。

照会の電文（余力・建玉・注文一覧・時価）は通信エラーで 1 度だけ送り直す。新規注文は送り直さない
（届いていた場合に二重発注になる。送信結果不明は台帳に `PENDING` で残り、次の回が当日の注文一覧で判定する）。
立花への電文は 1 本 1 行で `state/logs/daytrade-prod.jsonl` に残る（`code` = `broker.request`。何を送って、
何ミリ秒待って、何が返ったか。[LOGGING.md](LOGGING.md)）。

### 日次レポート（Discord）

レポートの行（`deploy/report.sh`）だけは他と前提が違う。回す前に 2 つ要る。

1. **通知先**。Discord へは **Bot の REST API** で送る（Incoming Webhook は使わない。
   Webhook は通常のテキストチャンネルでスレッドを作れず、投稿が平積みになるため）。
   Developer Portal で Bot を作り、サーバーに「チャンネルを見る」「メッセージを送信」
   「公開スレッドの作成」「スレッドでメッセージを送信」の権限で招待して、`.env` に
   `WBJP_DISCORD_BOT_TOKEN=…` と送り先のチャンネル ID を入れる。

   | 変数 | 送るもの |
   |---|---|
   | `WBJP_ALERT_CHANNEL_ID` | 異常（`jquants repair --notify`、`daytrade close` の失敗など） |
   | `WBJP_REPORT_CHANNEL_ID` | 17:35 の日次レポート。無ければ `WBJP_ALERT_CHANNEL_ID` に流れる |

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
| 日次 | 平日 17:35 | その日 | `state/reports/daily-<日付>.md` | 45 日 |
| 週次 | 金 21:30 | その週の月〜金 | `state/reports/weekly-<開始日>.md` | 180 日 |
| 月次 | 毎月 1 日 22:00 | 前月まるごと | `state/reports/monthly-<YYYY-MM>.md` | 消さない |

日次は `.claude/agents/daily-report.md`、週次・月次は `.claude/agents/periodic-report.md`
が「何をどの順で読むか」を決める。週次・月次は**日次レポートの .md に依存しない**
——追記専用の履歴（Parquet）とダイジェストを期間で読むので、日次の保持期間より
長い範囲でも振り返れる。仕組みは [FEEDBACK.md](FEEDBACK.md)「4. レポート（Discord）」。

vault（`~/obsidian-vault`）への commit は**そのノートだけ**を対象にする（`git commit -- <path>`）。
人が vault で add しかけていた変更は巻き込まない。commit / push に失敗したら cron のログに書き、
Discord（`WBJP_ALERT_CHANNEL_ID` / レポートの送り先）にも短く流す。

### cron の環境（時刻帯・MAILTO・WBJP_ENV）

- **時刻帯。** `deploy/crontab.txt` の時刻はすべて JST。Ubuntu / Debian の cron（3.0pl1）は
  `CRON_TZ` を読まず**システムの時刻帯**でスケジュールするので、マシンが Asia/Tokyo であること
  （`timedatectl`）が前提。`CRON_TZ=Asia/Tokyo` は cronie（RHEL 系）に移したとき効くように、
  `TZ=Asia/Tokyo` はコマンド側の `date` などが JST で動くように置いてある。
- **MAILTO。** ログへリダイレクトしていても、Go が起動する前の失敗（`cd` の失敗、`state/logs` が
  無い、`bin/` が無い）は cron がメールで送るしかない。このマシンには MTA（`sendmail`）が無いので、
  宛先を入れる前に `msmtp-mta` などを入れること。crontab の `# MAILTO=` は宛先が決まるまでコメントのまま。
- **外の死活監視（Mackerel）。** 通知はどれもこのマシンから Discord へ出るので、マシン停止・ネット断・
  cron 停止では何も鳴らない。**来なかったら向こうが知らせる**役は Mackerel に寄せる（組織 `pimopimo`、サービス `jstock`）。
  - マシン停止・ネット断 … mackerel-agent の connectivity 監視（もとから動いている）。
  - cron 停止 … `deploy/mackerel-alive.sh` が 5 分おきに `alive.cron` = 1 を投稿する。20 分途切れたら警報。
  - 点検の失敗・未実行 … 9:22 の朝の点検と 15:40 の verify の後に `deploy/ping.sh <KEY> <終了コード>` が
    `state/ping/<KEY>` に印（日付 時刻 終了コード）を残す。`mackerel-alive.sh` がそれを読んで
    `alive.morning` / `alive.verify` を投稿する。0 = 正常、1 = 終了コードが 0 以外、
    2 = 平日の期限（9:40 / 16:00）を過ぎても今日の印が無い。0 を超えたら警報。休場日も印は残る。
  - API キーは `.env` の `MACKEREL_APIKEY`、無ければ `/etc/mackerel-agent/mackerel-agent.conf` から読む。
    監視ルールは Mackerel 側にある（名前が `jstock:` で始まる 3 本）。通知先は Mackerel の通知チャンネル。
  - `ping.sh` は `.env` に `HEALTHCHECK_URL_<KEY>` があれば healthchecks.io 型の URL へも打つ（未設定なら打たない）。
- **打ち切り・見送りの通知（`WITH_LOCK_NOTIFY=1`）。** `with-lock.sh` の `[timeout]`・`[killed]`・`[lock_busy]` はログに
  書くだけだと誰も読まない（朝の点検は open と snap だけ）。open / guard / close / verify / plan の行は
  `WITH_LOCK_NOTIFY=1` を立て、`bin/discord-post` で知らせる。snap のように打ち切り・見送りが設計のうちの行には立てない。
- **WBJP_ENV（口座）。** Go の既定は `uat`、以前の `report.sh` / `night-repair.sh` / `morning-check.sh`
  の既定は `prod` で食い違っていた。既定で補うと、どちらに揃えても「黙って別の口座の
  ダイジェストを読む（あるいは 1 件も無くて何もしない）」が起きるので、**3 本とも未設定なら
  終了コード 2 で止まる**。crontab の行は `WBJP_ENV=prod` を渡す（`report.sh` と `morning-check.sh` は
  `.env` の値でもよい）。手で回すときも `WBJP_ENV=prod DRY_RUN=1 deploy/report.sh daily` のように付ける。

### 夜間自己修復（night-repair）

`deploy/night-repair.sh`（6:00）はダイジェストに異常があるときだけ `claude -p --agent night-repair` を起こす。
本番の作業ツリーは 8:30〜 の cron が `config/daytrade_margin/*.toml` と `bin/` を読むので、次を守らせている。

- **修正は `/tmp/night-repair-<YYYYMMDD>-<slug>` の git worktree の中**で行い、`auto-fix/…` ブランチを
  push して PR を作るまで。本番の作業ツリーでは checkout / pull をしない（以前の手順は
  `git checkout -b` をこの木でやっていて、失敗すると別ブランチの config のまま朝の cron が走りえた）。
- 禁止事項は `.claude/agents/night-repair.md` に書き、`--disallowedTools` でも止める
  （`deploy/build.sh` のどの呼び方も、`crontab`、`--live` と発注系のコマンド、本番の作業ツリーを動かす
  git、`main` への push・強制 push、`gh pr merge`、本番の作業ツリーと worktree の `config/` への書き込み）。
  Claude Code の規則は境界ではない（別の書き方で抜けうる）ので、終了後にスクリプトが本番の作業ツリーの
  ブランチ・HEAD・未コミットの変更を実行前と比べ、動いていればレポートの頭に警告を書く
  （汚れていなければ元のブランチに戻す）。残った worktree は片付ける。
- `.env` は丸ごと export しない。claude の子に渡すのは `WBJP_ENV` とメモリの上限だけで、
  Discord の送信だけがサブシェルで `.env` を読む。`bin/*` の Go は自分で `.env` を読むので、
  エージェントが叩く `review` などは困らない。

### 置き場の掃除（prune-state）

`deploy/prune-state.sh`（日曜 4:30）は、それまで掃除の仕組みが無かった置き場を保持の決まりで片付ける。
何が消えるかは `deploy/prune-state.sh --dry-run` で見られる（何も書き換えない）。結果は `state/logs/prune-state.log`。

| 置き場 | 決まり |
|---|---|
| `state/logs/*.log`（cron の `>>` が書く素のログ） | 10MB を超えたら `<name>.log.1` へ退避（前の `.1` は上書き）。rename なので書きかけの行を失わない。JSONL（`*.jsonl`）は Go 側が日次で退避し 90 日で消すので触らない |
| `state/digest/<env>-<日付>.jsonl` | 日付が 400 日より前なら消す |
| `state/backup/crontab/crontab-*.txt`・`state/logs/crontab.backup.*`（以前の置き場） | 90 日より前で、新しい方から 20 世代に入らないものを消す |
| `data/jquants/_raw/` | **消さない。** 一括ダウンロードの原本で、J-Quants は 10 年より前を返さなくなるので再取得できない（`docs/JQUANTS_ARCHIVE.md`「冪等性・安全」） |

名前から日付を読めないファイルは触らない。日数・世代数・上限は `PRUNE_DIGEST_DAYS`・`PRUNE_CRONTAB_DAYS`・
`PRUNE_CRONTAB_KEEP`・`PRUNE_LOG_MAX_MB` で変えられる。state の SQLite の複製（`state/backup/*.db`）は
`accum backup` が世代を管理する（既定 30 世代）ので対象にしない。

### cron を入れる前の検証

cron 専用の「実行せずに全部検証する」コマンドは無い。次の 3 段で確かめる:

```bash
# 1. 構文チェックと差分。差し替え後の全体を `crontab -n`（投入せずに構文だけ検査）に通し、いまの crontab との
#    差分を見せる（--dry-run でもバックアップは state/backup/crontab/ に 1 つ増える）
deploy/install-crontab.sh --dry-run
#    **`crontab <ファイル>` で直接入れない**——同居している他のジョブが消える（「4. 定期実行」）。
#    ファイル単体の検査は Debian / Ubuntu なら crontab -n <ファイル>、cronie（RHEL 系）なら crontab -T <ファイル>

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
意味。コマンドに書くなら `\%`）と、Go が起動する前の失敗（`cd` の失敗・リダイレクト先が無い）が
ログに残らないこと（上の「cron の環境」の `MAILTO`）。

- **1 回きりの cron を作らない。** 失敗すると次の機会（翌日・翌月）までバックアップや
  取り込みの無い状態が続くため、どのジョブも「何度叩いても同じ（冪等）」に作り、
  頻度を上げて失敗を次の実行で自動回復させる。失敗自体は Discord（`WBJP_ALERT_CHANNEL_ID`）に通知される
  - `accum backup` は毎日 16:37 と 22:37（場中を避けた）。同日分は上書きなので世代は 1 日 1 つのまま。
    16:37 に失敗しても 22:37 に取り直し、失敗するたびに通知が来る
  - 月次の `jquants backfill` は 2〜5 日の 4 回。成功していれば 2 回目以降は
    更新された一括ファイル（過誤訂正、月末数日の取り漏れ）だけを取り直す
- 平日 20:00 の `jquants repair --notify` は欠けの監視と自動修復。当日ぶん（16:30〜18:00 公開）が
  取れていなければその場で取り直し、それでも埋まらなかった日だけ Discord（`WBJP_ALERT_CHANNEL_ID`）に
  通知する（未設定ならログのみ）。通知が来たら手で `jquants repair --dry-run` → `jquants repair`
  （分足・ティックも見るなら cron と同じく `JQUANTS_MINUTE_BARS=1 JQUANTS_TICKS=1` を付ける）。
  `jquants check` は取り直さずに見るだけ

- `accum backup` は `state/` の全 SQLite（デイトレの台帳 `daytrade-prod.db`、積立台帳 `accum-prod.db`、
  スイング売買の記録 `wbjp-prod.db`）を `state/backup/<名前>-YYYYMMDD.db` に複製する（各 30 世代、SQLite の
  オンラインバックアップなので実行中でも一貫する）。
  台帳は「今月いくら発注済みか」の唯一の記録で失うと当月を買い直すので、`state/backup/` は
  別ディスクやオブジェクトストレージへ同期しておく。`accum run` は起動時にブローカーの当月の
  約定と台帳を突き合わせ、台帳に無い約定があれば発注を止めて通知する
- 選定の履歴 `state/daytrade/history/` と `state/wbjp/history/`（追記専用の Parquet、
  `docs/DAYTRADE.md`「履歴」）は `accum backup` の対象外。ファイルは増えるだけで書き換わらないので、
  `state/backup/` と一緒に `rsync -a`（`--delete` なし）で別ホストへ同期する
- 平日 17:05・17:20・20:20 の `daytrade evaluate` は、朝の候補（選んだ銘柄も次点も）に当日の日足を当てて
  `state/daytrade/history/evaluation/` に残す（17:05 は 17:35 の日次レポートに間に合わせるため、20:20 は公開が
  遅れた日の拾い直し。同じ日を何度評価しても最後の 1 回だけが使われる）。日足が取り込まれる前に走れば何もせず終わる
  （翌日に `--date` 無しでは前日を拾わないので、抜けた日は手で `--date` を付けて回す）。
  選定の妥当性は `daytrade review` で見る（`docs/DAYTRADE.md`）
- `accum run --live` は発注時間帯（既定 14:00〜15:00 JST、土日除く）の外では足の同期も
  ブローカー接続もせずに終了する。`--live` 無しの dry-run は確認用なので、いつでも判断まで見せる

- `flock -n` は前回の実行がまだ終わっていない場合に二重起動しないためのロック（万一20分より処理が長引いた時の保険。ファイルは初回に自動作成される）。デイトレの行は `deploy/with-lock.sh` 経由で、ロックを取れずに見送った回もログに残る
- ログを見て「発注時間帯の外」「新規足なし」ばかりなら正常（判断はしているが何もしていないだけ）

## 4-b. 発注経路を開ける前に

注文照会（`CLMOrderList`）・信用建玉・信用の発注（買建・売建、指値・成行の新規と返済）は 2026-09-11〜14 に
本番口座で確かめ、デイトレの発注経路は 2026-09-15 から開けている。**まだ実機で確かめていない電文**（前営業日の
注文照会、寄成・寄指、引けの保険注文など）は [BROKER_VERIFY.md](BROKER_VERIFY.md)「未検証の電文」にある。
応答の形が違えば必ず失敗する作りにしてある（同「設計: 分からないときは必ず止まる」）。新しい電文を本番に
出す前と、`wbjp` / `accum` の発注経路を開ける前は、同じ文書の手順で確かめる。

## 5. 緊急停止

設定の `kill_switch = true` で即停止（cron は消さなくてよい。`--live` があっても発注しない。設定は毎回読むので
`build.sh` も要らない）。場所はプロジェクトごとに違う:

| プロジェクト | ファイル | 項目 |
|---|---|---|
| デイトレ（`daytrade`） | `config/daytrade/daytrade.toml`（`config/daytrade_margin` はここを `extends` する） | `[execution]` の `kill_switch` |
| 積立（`accum`） | `config/accum/accum.toml` | 先頭（`[execution]` の前）の `kill_switch` |
| スイング（`wbjp`） | `config/settings.toml`（`--config-dir` で渡す設定の `settings.toml`） | `[risk]` の `kill_switch` |

- **デイトレの `kill_switch` は `open` だけでなく `close` / `guard` / `protect` の発注も止める**（どれも同じ
  `CanExecuteLive` を通る）。建玉があるときに立てると引けの手仕舞いが出ず、持ち越しになる——場中に立てたなら、
  建玉は立花の画面から手で返済する（立てる前に `daytrade protect` が置いた引けの保険注文はブローカーに残る。保険は後場（12:31〜）だけなので、前場に立てたときはまだ無い。
  実機での挙動は未検証——`docs/BROKER_VERIFY.md`「引けの保険注文」）
- crontab ごと止めるときは `state/crontab.paused` を作ってから（無いと `jstock-guard` が `state/crontab.good` から
  戻す。上の「cron とは別系統の実行役」）。`jstock-close-net` まで止めるなら `deploy/install-systemd.sh --remove`
- 実行ファイルが原因なら `deploy/rollback-bin.sh` で 1 世代前（`bin/.prev`）へ戻す。設定が実行ファイルより
  新しいときは戻すと悪化するので、`deploy/build.sh` で作り直す（「6. 更新」）

## 6. 更新（GitHub から取り込む）

```bash
cd /home/abobo/jstock-go
flock /tmp/accum-run.lock git pull --ff-only   # 走行中の accum run と重ならないようにロックを取る
make ci                                        # 任意: 整形の確認 + コンパイルの確認 + vet + staticcheck + test（push 時の CI と同じ。bin/ は作らず、作業ツリーも書き換えない）
flock /tmp/accum-run.lock deploy/build.sh      # 実行ファイルを作り直す（走行中と重ならないように）
```

- **cron の再起動は不要**。どのジョブも毎回新しいプロセスで起動するので、`build.sh` を回した次の実行から新コードになる
- **`build.sh` を忘れると古い実行ファイルのまま動く**。`git pull` だけでは反映されない（Go は実行ファイルに固める）
- **設定（`config/`）に項目を足したコミットでは `build.sh` が必須。** 設定の読み込みは strict
  （`DisallowUnknownFields`）なので、古い実行ファイルは知らない項目を見た時点で
  `strict mode: fields in the document are missing in the target struct` で落ちる
  ——`plan` `snap` `open` `close` `guard` `verify` が**全部**、設定を読む段階で止まる。
  2026-09-19 に `preopen_legs` / `extra_symbols` を足して `crontab` は入れたのに `build.sh` を
  忘れ、次の営業日（9/24）の朝が丸ごと止まる状態で数時間置いた。**確かめ方**:
  `./bin/daytrade preflight --config-dir config/daytrade_margin` が「問題なし」を出すこと
  （設定・今日の plan・台帳・ディスクを読むだけ。cron でも 8:40 に回り、問題があれば通知する）
  `accum` の設定（`config/accum/accum.toml`）も 2026-09-24 から同じ strict で、加えて予算・`market`・`tax_account_type`・`lot_size_overrides` の読めない値はエラーになる（以前は既定に倒していた）。止まるのは `accum sync` `evaluate` `run` など設定を読むコマンド。確かめ方は `go test ./pkg/accum/config/`（`TestRepositoryConfigLoadsStrictly` がリポジトリの設定を読む）
- `build.sh` は `bin/` へ直接ビルドしない。別名に作って**全部できてから** `mv` で差し替えるので、
  場中に回しても cron が書きかけの実行ファイルを掴まない（途中で失敗したら古い一式のまま）
- `data/`（足・アーカイブ）と `state/`（台帳・ログ・バックアップ）は git 管理外なので pull で消えない
- `config/` は git 管理下。サーバー側で `accum.toml` を直接編集すると pull が衝突する。
  設定変更は **ローカルで commit → push → サーバーで pull** の一方向に揃える
- `--ff-only` が失敗する（履歴が書き換えられた等）ときは、サーバーに手元の変更が無いことを
  `git status` で確かめてから `git fetch origin && git reset --hard origin/main`
