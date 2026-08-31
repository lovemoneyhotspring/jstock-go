"""環境変数由来の設定と、実発注の可否判定。

``settings.toml`` のようなファイル設定はプロジェクトごとに違う（スイング
売買はリスク上限やサイジング、積立は予算と銘柄）。ここにあるのは
どちらでも同じ「環境」の話だけ: どの口座（uat / prod）に繋ぐか、
データをどこに置くか、実弾を撃ってよいか。

置き場は 2 つに分かれる:
    - ``data_dir``（既定 ``data/``）: 足・財務・J-Quants アーカイブなど、
      取得元から再取得できるキャッシュ。**ホスト間で丸ごとコピーしてよい**
    - ``state_dir``（既定 ``state/``）: 発注の台帳・ログ・バックアップ。
      そのホストで起きたことの唯一の記録で、**他のホストのファイルで
      上書きしてはいけない**
"""

from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict

from wbcore.credentials import ENDPOINTS, Endpoints, Environment
from wbcore.logging import get_logger

_log = get_logger(__name__)


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
    #: サーバー固有の状態の置き場（``WBJP_STATE_DIR``）。発注の台帳・ログ・
    #: 台帳のバックアップなど、**別のホストのファイルで上書きしてはいけないもの**を
    #: ここに置く。``data_dir`` は逆に、取得元から再取得できるキャッシュと
    #: アーカイブだけにし、ホスト間で丸ごとコピーしてよい。
    state_dir: Path = Path("state")
    log_level: str = "INFO"
    #: 構造化ログを JSON で出す（本番運用向け）
    log_json: bool = False
    #: 画面に時刻を出すときの時間帯（``WBJP_TIMEZONE``）。既定は UTC。
    #: 保存と演算は常に UTC で、ここは表示にだけ効く。表示には必ず略号が付く。
    timezone: str = "UTC"
    #: ログの置き場（``WBJP_LOG_DIR``）。**ファイルに残すログはここ 1 箇所だけ。**
    #: 省略時は ``data_dir / "logs"``——データの置き場を変えればログも一緒に動く。
    #: 置き場が分散すると障害時にどこを見ればよいか分からなくなるため、
    #: 機械が読む JSONL も、cron / systemd で stderr を残す場合もここに集める。
    #: SDK が勝手に作るログ（``webull_*_sdk.log``）は抑止してあり、どこにも書かない。
    log_dir: Path | None = None

    @property
    def resolved_log_dir(self) -> Path:
        """実際に使うログの置き場。``WBJP_LOG_DIR`` があればそれ、無ければ ``state_dir/logs``。"""
        return self.log_dir if self.log_dir is not None else self.state_dir / "logs"

    def log_file(self, app: str) -> Path:
        """アプリ（wbjp / accum）と環境ごとの JSONL。日次でローテーションする。"""
        return self.resolved_log_dir / f"{app}-{self.env.value}.jsonl"

    @property
    def endpoints(self) -> Endpoints:
        return ENDPOINTS[self.env]

    @property
    def db_path(self) -> Path:
        """スイング売買の記録（注文・シグナル・実行履歴）。"""
        return self._stateful(f"wbjp-{self.env.value}.db")

    @property
    def accum_db_path(self) -> Path:
        """積立の発注台帳。「今月いくら発注済みか」の唯一の記録。"""
        return self._stateful(f"accum-{self.env.value}.db")

    @property
    def daytrade_db_path(self) -> Path:
        """デイトレの発注台帳（``daytrade``）。当日の買いと手仕舞いを実行をまたいで覚える。"""
        return self._stateful(f"daytrade-{self.env.value}.db")

    @property
    def daytrade_dir(self) -> Path:
        """デイトレの候補リスト（前夜の ``daytrade plan`` の出力）の置き場。"""
        return self._stateful("daytrade", directory=True)

    @property
    def backup_dir(self) -> Path:
        """台帳バックアップの置き場（``accum backup``）。"""
        return self._stateful("backup", directory=True)

    def _stateful(self, name: str, *, directory: bool = False) -> Path:
        """状態ファイルの置き場。旧配置（``data_dir`` 直下）からは自動で移す。

        以前は台帳を ``data/`` に置いていた。移行を運用者の手作業に任せると、
        pull 直後の cron が**空の台帳**で走って当月を買い直す。新しい場所に
        無く旧い場所にあるなら、ここで移動して事故を断つ。
        """
        import shutil

        new_path = self.state_dir / name
        old_path = self.data_dir / name
        if not new_path.exists() and old_path.exists() and old_path.resolve() != new_path.resolve():
            new_path.parent.mkdir(parents=True, exist_ok=True)
            shutil.move(str(old_path), str(new_path))
            _log.warning(
                "状態ファイルを新しい置き場に移しました",
                code="settings.state_migrated",
                src=str(old_path),
                dest=str(new_path),
            )
        if directory:
            return new_path
        return new_path

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
