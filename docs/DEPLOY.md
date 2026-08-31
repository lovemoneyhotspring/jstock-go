# 本番デプロイ チェックリスト

詳細は README.md の「APIキーの置き場所」「cron で回す」参照。ここは持っていく物と手順の要約だけ。

## 1. サーバーに持っていく物

| 種類 | 場所 | git管理 |
|---|---|---|
| コード | `src/` `pyproject.toml` `uv.lock` `.python-version` | ✅ |
| 設定 | 運用する戦略の `config/<戦略名>/` 一式 | ✅ |
| 秘密情報 | Webull APIキーと J-Quants APIキー（`.env` または systemd `EnvironmentFile=`） | ❌ 別途用意 |
| キャッシュ | `data/`（足・財務・J-Quants アーカイブ） | ❌ 別ホストからコピーしてよい（または再取得） |
| 状態 | `state/`（発注台帳・ログ・バックアップ） | ❌ **このホスト固有。他ホストのファイルで上書き厳禁** |

`config/` と `data/` は `src/` の外にあるので、**`src/` だけ配ると動かない**。

## 2. セットアップ手順

> **先に IP を許可する。** Webull OpenAPI の API キーは送信元 IP で制限される。
> サーバーの外向き IP（`curl -s https://ifconfig.me`）を Webull の API キー設定の
> 許可リストに追加していないと、接続が `IP_NOT_ALLOWED`（HTTP 401）で失敗する。
> UAT と本番でキーが別なら、本番キーの側に登録する。

```bash
# 1. コード配置
git clone https://github.com/lovemoneyhotspring/we-bull.git /home/abobo/webull/wbjp && cd /home/abobo/webull/wbjp
# private リポジトリなので認証が必要。HTTPS の場合はパスワード欄に PAT（Personal Access Token）を使う。
# SSH鍵を使うなら代わりに: git clone git@github.com:lovemoneyhotspring/we-bull.git /home/abobo/webull/wbjp

# 2. 依存関係
uv sync

# 3. APIキー（本番口座）
# ヘッドレスLinuxにはkeyringが無いので `credentials set` は使わず、
# systemd 経由で環境変数として渡す。
sudo install -d -m 750 -o root -g wbjp /etc/wbjp
sudo install -m 640 -o root -g wbjp /dev/null /etc/wbjp/wbjp.env
sudo vi /etc/wbjp/wbjp.env
# ↑ このファイルに以下を書く（値は自分のAPIキーに差し替え）
#   WBJP_PROD_APP_KEY=...
#   WBJP_PROD_APP_SECRET=...
#   WBJP_PROD_ACCOUNT_ID=...
#   WBJP_JQUANTS_API_KEY=...   # 日本株の足（J-Quants）。環境で分けない
#
# systemdサービス定義（/etc/systemd/system/wbjp.service）に
# EnvironmentFile=/etc/wbjp/wbjp.env を1行書けば、起動時にこのファイルの
# 中身がプロセスの環境変数として読み込まれる（cronで動かす場合は不要、
# 4節のcron行で直接 .env を読ませる）

# 4. cron のリダイレクト先を先に作る（アプリが作るのは起動後のため）
mkdir -p state/logs

# 5. 価格データの初期取り込み（運用する戦略のconfigごとに）
uv run wbjp data sync --config-dir config/us --days 1500

# 6. dry-runで確認（--live無しは常に発注しない）
WBJP_ENV=prod uv run wbjp run --config-dir config/us
```

## 3. `.env` の置き場所

- **既定**: リポジトリ直下（カレントディレクトリ基準）= `/home/abobo/webull/wbjp/.env`。`chmod 600` 必須（緩いと起動時に警告）
- **絶対パスで指定したい場合**: 環境変数 `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` を渡せば、cronの`cd`忘れがあってもそこを読む
- 中身は秘密でない項目（`WBJP_ENV=prod` 等）のみ。APIキー自体は上記手順3のsystemd `EnvironmentFile=`か`credentials set`（keyring）経由が推奨

## 4. 定期実行（cron）— 20分おき固定、時刻計算はしない

`wbjp run` と `accum run` はどちらも「呼ばれた時点で必要な分だけ判断し、不要なら何もしない」
作りになっている。

- 注文IDは日付・銘柄・数量から決定論的に作られる → 何度呼んでも二重発注しない
- `accum run` は発注してよい時間帯（既定14:00〜15:00 JST）の外では自動でスキップする
- 新しい確定足が無ければ`wbjp run`は前回と同じ判断になるだけ（無害）

なので**市場が開いているか・引けたかを cron 側で計算する必要はない**。固定間隔で叩き続ければ、
時間帯の判定は各コマンドの中で完結する。

開始の分は**互いにずらす**。同時に走ると yfinance が同一 IP からのバーストとして
遮断しやすく、同じ銘柄の足を二つのプロセスが同時に書く競合も避けられる。
`sleep` でずらすより cron の分指定の方が、crontab を見ただけでタイミングが分かる。

```cron
# wbjp: 0,20,40 分 / accum: 7,27,47 分 / jquants: 13,43 分（互いに重ねない）
*/20 * * * *    cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/wbjp-run.lock  /home/abobo/webull/wbjp/.venv/bin/wbjp  run --live --yes --config-dir config/us >> state/logs/wbjp-run.log  2>&1
7-59/20 * * * * cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/accum-run.lock /home/abobo/webull/wbjp/.venv/bin/accum run --live --yes                       >> state/logs/accum-run.log 2>&1
13,43 * * * *   cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/jquants.lock   /home/abobo/webull/wbjp/.venv/bin/jquants sync                                >> state/logs/jquants-sync.log 2>&1
37 * * * *      cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock    /tmp/accum-run.lock /home/abobo/webull/wbjp/.venv/bin/accum backup                                >> state/logs/accum-backup.log 2>&1
50 3 2-5 * *    cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/jquants.lock   /home/abobo/webull/wbjp/.venv/bin/jquants backfill                             >> state/logs/jquants-backfill.log 2>&1
0 20 * * 1-5    cd /home/abobo/webull/wbjp && WBJP_ENV=prod                              /home/abobo/webull/wbjp/.venv/bin/jquants check --notify                       >> state/logs/jquants-check.log 2>&1
# daytrade（docs/DAYTRADE.md）: 前夜 20:30 に候補、9:01 から寄付買い、15:20 から引け売り。open/close は時間帯の外では何もしない
30 20 * * 1-5   cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/daytrade.lock  /home/abobo/webull/wbjp/.venv/bin/daytrade plan                                >> state/logs/daytrade-plan.log 2>&1
1,4,7 9 * * 1-5 cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/daytrade.lock  /home/abobo/webull/wbjp/.venv/bin/daytrade open --live --yes                   >> state/logs/daytrade-open.log 2>&1
20,24,28 15 * * 1-5 cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/daytrade.lock /home/abobo/webull/wbjp/.venv/bin/daytrade close --live --yes               >> state/logs/daytrade-close.log 2>&1
```

`daytrade open` は 9:01・9:04・9:07 の 3 回呼ぶ（板寄せ直後は気配の約定時刻が前日のままで「古い」と判定されることがあるため、9:00 ちょうどは避ける。気配を取れなかった回の再試行で、台帳に買いがあれば以降は何もしない）。
`close` は 15:20・15:24・15:28 の 3 回（1 回目で売れていれば 2 回目以降は何もしない。拒否されていれば送り直す）。15:20 の成行はその場の気配で約定し、15:25 以降ならクロージング・オークションで引け値になる。祝日は `open` が「候補なし／気配なし」で終わるだけで無害。

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
mkdir -p /home/abobo/webull/wbjp/state/logs

# 3. コマンド部分の検証は 1 回手で流すしかない。cron と同じ最小環境を再現して実行する
#    （--live は外す。発注以外のデータ取得・判断・記録はすべて動く）
cd /home/abobo/webull/wbjp && env -i HOME=$HOME PATH=/usr/bin:/bin WBJP_ENV=prod \
  .venv/bin/wbjp run --config-dir config/us
cd /home/abobo/webull/wbjp && env -i HOME=$HOME PATH=/usr/bin:/bin WBJP_ENV=prod \
  .venv/bin/jquants sync --dry-run
```

構文チェックが見てくれるのは「5 つの時刻フィールド＋コマンドの形」だけで、
パスの誤り・権限・環境変数はコマンドを実際に流さないと分からない。
`env -i` で流すのは、対話シェルの PATH や .zshrc に助けられて「手では動くが
cron では動かない」を潰すため。ほかに cron 固有の罠は `%` （cron では改行の
意味。コマンドに書くなら `\%`）と、`MAILTO` 未設定時にエラーメールが捨てられる
こと（このリポジトリは stderr をログへリダイレクトしているので影響しない）。

- **1 回きりの cron を作らない。** 失敗すると次の機会（翌日・翌月）までバックアップや
  取り込みの無い状態が続くため、どのジョブも「何度叩いても同じ（冪等）」に作り、
  頻度を上げて失敗を次の実行で自動回復させる。失敗自体は `WBJP_ALERT_WEBHOOK_URL` に通知される
  - `accum backup` は毎時（37 分）。同日分は上書きなので世代は 1 日 1 つのまま。
    1 回失敗しても 1 時間後に取り直し、失敗が続けば毎時通知が来る
  - 月次の `jquants backfill` は 2〜5 日の 4 回。成功していれば 2 回目以降は
    更新された一括ファイル（過誤訂正、月末数日の取り漏れ）だけを取り直す
- 平日 20:00 の `jquants check --notify` は欠けの監視。当日ぶん（16:30〜18:00 公開）が
  取れていなければ `WBJP_ALERT_WEBHOOK_URL` に通知する（未設定ならログのみ）

- `accum backup` は `state/` の全 SQLite（積立台帳 `accum-prod.db` とスイング売買の記録
  `wbjp-prod.db`）を `state/backup/<名前>-YYYYMMDD.db` に複製する（各 30 世代、SQLite の
  オンラインバックアップなので実行中でも一貫する）。
  台帳は「今月いくら発注済みか」の唯一の記録で失うと当月を買い直すので、`state/backup/` は
  別ディスクやオブジェクトストレージへ同期しておく。`accum run` は起動時にブローカーの当月の
  約定と台帳を突き合わせ、台帳に無い約定があれば発注を止めて通知する
- `accum run --live` は発注時間帯（既定 14:00〜15:00 JST、土日除く）の外では足の同期も
  ブローカー接続もせずに終了する。`--live` 無しの dry-run は確認用なので、いつでも判断まで見せる

- `flock -n` は前回の実行がまだ終わっていない場合に二重起動しないためのロック（万一20分より処理が長引いた時の保険。ファイルは初回に自動作成される）
- ログを見て「発注時間帯の外」「新規足なし」ばかりなら正常（判断はしているが何もしていないだけ）

## 5. 緊急停止

`config/<戦略名>/settings.toml` の `risk.kill_switch = true` で即停止（cronは消さなくてよい）。

## 6. 更新（GitHub から取り込む）

```bash
cd /home/abobo/webull/wbjp
flock /tmp/accum-run.lock git pull --ff-only   # 走行中の accum run と重ならないようにロックを取る
uv sync                                        # 依存関係が変わっていれば反映（変更が無ければ一瞬）
uv run pytest tests/ -q                        # 任意: 動作確認
```

- **cron の再起動は不要**。`wbjp run` / `accum run` は 20 分ごとに新しいプロセスで起動するので、次の実行から新コードになる
- `data/`（台帳 DB・足・ログ）は git 管理外なので pull で消えない
- `config/` は git 管理下。サーバー側で `accum.toml` を直接編集すると pull が衝突する。
  設定変更は **ローカルで commit → push → サーバーで pull** の一方向に揃える
- `--ff-only` が失敗する（履歴が書き換えられた等）ときは、サーバーに手元の変更が無いことを
  `git status` で確かめてから `git fetch origin && git reset --hard origin/main`
