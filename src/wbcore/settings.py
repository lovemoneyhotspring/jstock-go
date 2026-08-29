"""環境変数由来の設定と、実発注の可否判定。

``settings.toml`` のようなファイル設定はプロジェクトごとに違う（スイング
売買はリスク上限やサイジング、積立は予算と銘柄）。ここにあるのは
どちらでも同じ「環境」の話だけ: どの口座（uat / prod）に繋ぐか、
データをどこに置くか、実弾を撃ってよいか。
"""

from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict

from wbcore.credentials import ENDPOINTS, Endpoints, Environment


class AppSettings(BaseSettings):
    """環境変数と .env から読む設定。ここに秘密は入れない。

    接頭辞は ``WBJP_``（``WBJP_ENV=prod`` など）。プロジェクトが分かれても
    口座とデータ置き場は共有するので、環境変数も共有する。
    """

    model_config = SettingsConfigDict(
        env_prefix="WBJP_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    env: Environment = Environment.UAT
    config_dir: Path = Path("config")
    data_dir: Path = Path("data")
    log_level: str = "INFO"
    #: 構造化ログを JSON で出す（本番運用向け）
    log_json: bool = False
    #: 画面に時刻を出すときの時間帯（``WBJP_TIMEZONE``）。既定は UTC。
    #: 保存と演算は常に UTC で、ここは表示にだけ効く。表示には必ず略号が付く。
    timezone: str = "UTC"
    #: 機械が読むログ（JSON Lines）の置き場（``WBJP_LOG_DIR``）。
    log_dir: Path = Path("data/logs")

    def log_file(self, app: str) -> Path:
        """アプリ（wbjp / accum）と環境ごとのログファイル。日次でローテーションする。"""
        return self.log_dir / f"{app}-{self.env.value}.jsonl"

    @property
    def endpoints(self) -> Endpoints:
        return ENDPOINTS[self.env]

    @property
    def db_path(self) -> Path:
        return self.data_dir / f"wbjp-{self.env.value}.db"

    @property
    def bars_dir(self) -> Path:
        return self.data_dir / "bars"


def allows_live_orders(
    env: Environment, live_flag: bool, *, kill_switch: bool = False
) -> tuple[bool, str]:
    """実発注してよいかを判定する。

    本番の実発注には ``WBJP_ENV=prod`` と ``--live`` の**両方**が要る。
    UAT は実弾ではないので ``--live`` だけで足りる。
    キルスイッチはすべてに優先する。

    Returns:
        (発注してよいか, 理由)
    """
    if kill_switch:
        return False, "キルスイッチが有効（設定の kill_switch = true）"
    if not live_flag:
        return False, "--live が指定されていない（dry-run）"
    if env.is_production:
        return True, "本番環境・--live 指定あり"
    return True, f"{env.value} 環境・--live 指定あり（実弾ではない）"
