# 本番デプロイ チェックリスト

詳細は README.md の「APIキーの置き場所」「cron で回す」参照。ここは持っていく物と手順の要約だけ。

## 1. サーバーに持っていく物

| 種類 | 場所 | git管理 |
|---|---|---|
| コード | `src/` `pyproject.toml` `uv.lock` `.python-version` | ✅ |
| 設定 | 運用する戦略の `config/<戦略名>/` 一式 | ✅ |
| 秘密情報 | APIキー（`.env` または systemd `EnvironmentFile=`） | ❌ 別途用意 |
| データ | `data/`（価格キャッシュ・台帳DB・ログ） | ❌ 初回は空でOK |

`config/` と `data/` は `src/` の外にあるので、**`src/` だけ配ると動かない**。

## 2. セットアップ手順

```bash
# 1. コード配置
git clone <repo> /opt/wbjp && cd /opt/wbjp

# 2. 依存関係
uv sync

# 3. APIキー（本番口座）
uv run wbjp credentials set --env prod
# ヘッドレスLinuxはkeyringが使えないので /etc/wbjp/wbjp.env に WBJP_PROD_APP_KEY 等を書き、
# systemd の EnvironmentFile= で渡す（README「APIキーの置き場所」参照）

# 4. 価格データの初期取り込み（運用する戦略のconfigごとに）
uv run wbjp data sync --config-dir config/us --days 1500

# 5. dry-runで確認（--live無しは常に発注しない）
WBJP_ENV=prod uv run wbjp run --config-dir config/us
```

## 3. 定期実行（cron）

```cron
CRON_TZ=Asia/Tokyo
# 大引け後、平日16:30
30 16 * * 1-5 cd /opt/wbjp && WBJP_ENV=prod /opt/wbjp/.venv/bin/wbjp run --live --yes --config-dir config/us >> data/logs/wbjp-run.log 2>&1
```

詰まりやすい点（詳細はREADME参照）:
- `--yes` 必須（cronにはstdinが無い）
- `cd` 必須（`config/` `data/` `.env` はカレントディレクトリ基準）
- `.venv/bin/wbjp` をフルパスで呼ぶ（`uv run` は使わない）

## 4. 緊急停止

`config/<戦略名>/settings.toml` の `risk.kill_switch = true` で即停止（cronは消さなくてよい）。
