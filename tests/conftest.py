"""テスト全体の前提。**本番の状態を書き換えさせない。**

リポジトリ直下の ``.env`` には運用の値が入っている（``WBJP_ENV=prod``、
``WBJP_LOG_DIR=/home/abobo/jstock/state/logs``）。:class:`wbcore.settings.AppSettings`
は ``.env`` を読むので、**何もしないとテストが本番の置き場に書く**。実際、
CLI を呼ぶテストが ``state/logs/*-prod.jsonl`` に数千行を書き込んでいた。

これは記録が汚れるだけでは済まない:

- 本番のログに ``/tmp/pytest-of-*`` を含む行が混ざり、運用の記録を後から
  読むとき（人も AI も）実運用の行と見分けられない
- 台帳（``state/*.db``）を触るテストがあれば、発注済みの記録を壊しうる

そこで、テストごとに置き場を ``tmp_path`` へ逃がし、口座も uat に倒す。
環境変数は ``.env`` より優先されるので、これで ``.env`` の値は届かない。

``data_dir`` は**あえて逃がさない**。J-Quants のアーカイブは取得元から
再取得できるキャッシュで、テストは読むだけだから。書き込むテストは
自分で ``WBJP_DATA_DIR`` を差し替える（``tests/test_data_health.py``）。
"""

from __future__ import annotations

from pathlib import Path

import pytest


@pytest.fixture(autouse=True)
def _isolate_state(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    """状態の置き場をテスト専用の場所に向ける。

    自分で置き場や口座を指定するテスト（``tests/test_log_dir.py`` など）は、
    この後に ``setenv`` / ``delenv`` すれば上書きできる。fixture の適用順は
    autouse が先なので、テスト側の指定が必ず勝つ。
    """
    from wbcore.settings import AppSettings

    monkeypatch.setenv("WBJP_ENV", "uat")
    monkeypatch.setenv("WBJP_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("WBJP_LOG_DIR", str(tmp_path / "state" / "logs"))
    # 環境変数を消すだけのテストがあると、その裏でリポジトリ直下の `.env`
    # （`WBJP_ENV=prod`、`WBJP_LOG_DIR=…/jstock/state/logs`）が顔を出す。
    # 実際それで本番のログが汚れていたので、ファイル自体を読ませない
    monkeypatch.setitem(AppSettings.model_config, "env_file", str(tmp_path / "absent.env"))
