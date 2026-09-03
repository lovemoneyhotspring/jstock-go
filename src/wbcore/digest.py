"""日次ダイジェスト——AI が最初に読む 1 ファイル。

**なぜ要るか**

運用の記録は JSONL のログ（``state/logs/<app>-<env>.jsonl``）に全部あるが、
1 日ぶんが数 MB になる。「今日ちゃんと動いたか」を知るためだけに、AI が
その全部を読むのは無駄が大きい。一方で「読まなくてよい行」を後から見分ける
のは難しい——動いただけの行と、判断した行が同じ形で並んでいるから。

そこでダイジェストは**逆向き**に作る。各実行が終わるときに、その実行を
1 行に畳んで ``state/digest/<env>-<日付>.jsonl`` に足す。AI はまずこれを読み、
異常（``anomalies``）が載っている実行だけ ``run_id`` でログに降りる。

    # 今日の全実行（1 日あたり 50KB 程度）
    cat state/digest/prod-2026-09-03.jsonl

    # 異常があったものだけ
    jq 'select(.anomalies)' state/digest/prod-2026-09-03.jsonl

**なぜ 1 実行 1 行の追記なのか**

cron のジョブは互いに時刻をずらしてあるが、重ならない保証は無い（``flock``
は同じジョブ同士にしか効かない）。1 日 1 個の JSON を読んで書き換える形に
すると、重なった瞬間に片方の記録が消える。1 行の追記なら、POSIX の追記は
アトミックなので競合しても壊れない。

**項目**

``ts_utc`` / ``app`` / ``env`` / ``command`` / ``run_id`` / ``outcome`` /
``dur_ms`` は必ず付く。それ以外は :func:`note` で足したものがそのまま平らに並ぶ
（入れ子にしないのは、読む側の絞り込みを簡単にするため）。
"""

from __future__ import annotations

import atexit
import json
import os
import time
from contextvars import ContextVar
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

#: ダイジェストの形式の版。項目を変えたら上げる。
DIGEST_SCHEMA = "wbjp.digest.v1"


@dataclass
class _Run:
    """いま走っている実行の集計。"""

    app: str
    env: str
    command: str
    run_id: str
    path: Path
    started: float = field(default_factory=time.monotonic)
    outcome: str = "ok"
    fields: dict[str, Any] = field(default_factory=dict)
    anomalies: list[str] = field(default_factory=list)
    written: bool = False


_current: ContextVar[_Run | None] = ContextVar("wbcore_digest_run", default=None)


def digest_path(state_dir: Path, env: str, day: str) -> Path:
    """その日のダイジェストの置き場。アプリを分けないのは、AI が
    「今日どう動いたか」を 1 ファイルで読めるようにするため。"""
    return state_dir / "digest" / f"{env}-{day}.jsonl"


def start_run(
    *,
    app: str,
    env: str,
    command: str,
    run_id: str,
    state_dir: Path,
    day: str | None = None,
) -> None:
    """実行の記録を始める。CLI の callback から 1 回だけ呼ぶ。

    終了時の書き出しは :mod:`atexit` に任せる。各コマンドの出口に
    書き出しを散らすと、``typer.Exit`` や早期 return の経路を必ず取りこぼすため。
    """
    from wbcore.clock import stamp_iso

    resolved_day = day or stamp_iso("UTC")[:10]
    run = _Run(
        app=app,
        env=env,
        command=command,
        run_id=run_id,
        path=digest_path(state_dir, env, resolved_day),
    )
    _current.set(run)
    atexit.register(flush)


def note(**fields: Any) -> None:
    """この実行の集計に項目を足す（同じ鍵は上書き）。

    数える対象は「後から見て意味のあるもの」だけにする。候補の件数、
    発注した件数、見送った件数など。全部を載せるとログの写しになる。
    """
    run = _current.get()
    if run is not None:
        run.fields.update(fields)


def add(**counters: int) -> None:
    """数える項目を足し込む（``note`` と違い加算）。"""
    run = _current.get()
    if run is None:
        return
    for key, value in counters.items():
        current = run.fields.get(key)
        run.fields[key] = (current if isinstance(current, int) else 0) + value


def anomaly(code: str, detail: str = "") -> None:
    """異常を記録する。**ここに載った実行だけを AI が深掘りする。**

    「いつもと違う」ことだけを載せる。時間帯の外で何もしなかった、
    同期して変化が無かった——といった正常な結果は載せない。
    """
    run = _current.get()
    if run is not None:
        run.anomalies.append(f"{code}: {detail}" if detail else code)


def fail(code: str, detail: str = "") -> None:
    """実行が失敗した。異常として記録し、``outcome`` を ``error`` にする。"""
    run = _current.get()
    if run is not None:
        run.outcome = "error"
    anomaly(code, detail)


def skipped(reason: str) -> None:
    """何もしなかった実行（休日・時間帯の外）。異常ではない。"""
    run = _current.get()
    if run is not None:
        run.outcome = "skip"
        run.fields["reason"] = reason


def flush() -> None:
    """1 行にして書き出す。2 回目以降は何もしない。"""
    run = _current.get()
    if run is None or run.written:
        return
    run.written = True

    from wbcore.clock import stamp_iso

    record: dict[str, Any] = {
        "schema": DIGEST_SCHEMA,
        "ts_utc": stamp_iso("UTC"),
        "app": run.app,
        "env": run.env,
        "command": run.command,
        "run_id": run.run_id,
        "outcome": run.outcome,
        "dur_ms": int((time.monotonic() - run.started) * 1000),
        **run.fields,
    }
    if run.anomalies:
        record["anomalies"] = run.anomalies

    line = json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n"
    try:
        run.path.parent.mkdir(parents=True, exist_ok=True)
        # O_APPEND の 1 回の write は、他の実行と重なっても行が混ざらない
        with open(run.path, "a", encoding="utf-8") as handle:
            handle.write(line)
            handle.flush()
            os.fsync(handle.fileno())
    except OSError:
        # ダイジェストは記録の付帯物。ここで実行を落とさない
        return


def reset() -> None:
    """テスト用。実行中の記録を捨てる。"""
    _current.set(None)
