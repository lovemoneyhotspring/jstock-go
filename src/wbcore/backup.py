"""``state/`` のバックアップ。

``state/`` にはそのホストで起きたことの唯一の記録（積立の発注台帳
``accum-<env>.db``、スイング売買の記録 ``wbjp-<env>.db``）があり、
ブローカーからは再構築できない。失うと当月を買い直す・履歴を失う。

方針:
    - ``state_dir`` 直下の ``*.db`` を**すべて**、SQLite のオンライン
      バックアップ API で複製する（cron が書いている最中でも一貫した
      スナップショットになる。ファイルコピーだと書き込み途中を写しうる）
    - 複製先は ``<dest>/<元の名前から .db を除いたもの>-YYYYMMDD.db``。
      1 日 1 世代、古い世代から削る
    - ``logs/`` は対象外。ログは日次ローテーション＋90 日保持で自衛して
      おり、失っても売買は壊れない
    - 複製先の既定は ``state/backup/``。同じディスクなので、ホストごと
      失う障害には別ホストへの rsync を併用する（DEPLOY.md 参照）
"""

from __future__ import annotations

import datetime as dt
import sqlite3
from dataclasses import dataclass
from pathlib import Path

from wbcore.clock import today_utc
from wbcore.logging import get_logger
from wbcore.settings import AppSettings

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class BackupResult:
    """1 回のバックアップの結果。"""

    copied: list[Path]
    removed: list[Path]


def backup_sqlite(source: Path, destination: Path) -> Path:
    """SQLite をオンラインバックアップで複製する。"""
    destination.parent.mkdir(parents=True, exist_ok=True)
    with sqlite3.connect(source) as origin, sqlite3.connect(destination) as copy:
        origin.backup(copy)
    return destination


def backup_state(
    settings: AppSettings,
    *,
    dest: Path | None = None,
    keep: int = 30,
    today: dt.date | None = None,
) -> BackupResult:
    """``state/`` の全 SQLite を世代付きで複製する。

    Args:
        dest: 複製先。既定は ``state/backup``。
        keep: 元ファイルごとに残す世代数。0 以下なら削らない。

    Returns:
        複製したファイルと、世代削減で消したファイル。
    """
    directory = dest or settings.backup_dir
    stamp = f"{(today or today_utc()):%Y%m%d}"
    copied: list[Path] = []
    removed: list[Path] = []
    for source in sorted(settings.state_dir.glob("*.db")):
        stem = source.stem
        target = backup_sqlite(source, directory / f"{stem}-{stamp}.db")
        copied.append(target)
        if keep > 0:
            old = sorted(directory.glob(f"{stem}-*.db"))[:-keep]
            for path in old:
                path.unlink()
            removed.extend(old)
    log.info(
        "state をバックアップ",
        code="state.backup",
        dest=str(directory),
        copied=[p.name for p in copied],
        removed=len(removed),
    )
    return BackupResult(copied=copied, removed=removed)
