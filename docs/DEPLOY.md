# 本番デプロイ チェックリスト

詳細は README.md の「APIキーの置き場所」「cron で回す」参照。ここは持っていく物と手順の要約だけ。

## 1. サーバーに持っていく物

| 種類 | 場所 | git管理 |
|---|---|---|
| コード | `src/` `pyproject.toml` `uv.lock` `.python-version` | ✅ |
| 設定 | 運用する戦略の `config/<戦略名>/` 一式 | ✅ |
| 秘密情報 | Webull APIキーと J-Quants APIキー（`.env` または systemd `EnvironmentFile=`） | ❌ 別途用意 |
| データ | `data/`（価格キャッシュ・台帳DB・ログ） | ❌ 初回は空でOK |

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

# 4. 価格データの初期取り込み（運用する戦略のconfigごとに）
uv run wbjp data sync --config-dir config/us --days 1500

# 5. dry-runで確認（--live無しは常に発注しない）
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

```cron
*/20 * * * * cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/wbjp-run.lock  /home/abobo/webull/wbjp/.venv/bin/wbjp  run --live --yes --config-dir config/us >> data/logs/wbjp-run.log  2>&1
*/20 * * * * cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/accum-run.lock /home/abobo/webull/wbjp/.venv/bin/accum run --live --yes                       >> data/logs/accum-run.log 2>&1
30 16 * * *  cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock    /tmp/accum-run.lock /home/abobo/webull/wbjp/.venv/bin/accum backup                                >> data/logs/accum-backup.log 2>&1
*/30 * * * * cd /home/abobo/webull/wbjp && WBJP_ENV=prod flock -n /tmp/jquants.lock    /home/abobo/webull/wbjp/.venv/bin/jquants sync                                >> data/logs/jquants-sync.log 2>&1
```

- `accum backup` は台帳 `data/accum-prod.db` を `data/backup/accum-prod-YYYYMMDD.db` に複製する（30 世代）。
  台帳は「今月いくら発注済みか」の唯一の記録で失うと当月を買い直すので、`data/backup/` は
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
