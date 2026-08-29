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
    """注文を出してよいかを判定する。

    **注文を出すかどうかは ``--live`` だけで決まる。** 無ければデータ取得・判断・
    記録は行い、注文だけ出さない。``WBJP_ENV`` は「どの口座に繋ぐか」
    （uat＝テスト口座 / prod＝本番口座）であって、売買の可否ではない。
    キルスイッチはすべてに優先する。

    Returns:
        (発注してよいか, 理由)
    """
    if kill_switch:
        return False, "キルスイッチが有効（設定の kill_switch = true）"
    if not live_flag:
        return False, "--live なし（データ取得と判断は行い、注文は出さない）"
    return True, "--live あり"


def describe_mode(env: Environment, live_flag: bool, *, kill_switch: bool = False) -> str:
    """実行の冒頭に出す 1 行。口座と発注の可否を混同しないように、両方を並べて示す。

    例: ``口座: 本番（WBJP_ENV=prod）  発注: しない（--live なし）``
    """
    account = "本番口座" if env.is_production else "テスト口座（実弾ではない）"
    allowed, reason = allows_live_orders(env, live_flag, kill_switch=kill_switch)
    orders = "する" if allowed else "しない"
    return f"口座: {account}（WBJP_ENV={env.value}）  発注: {orders}（{reason}）"
