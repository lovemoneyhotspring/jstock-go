"""レート制限。

証券会社の API には「残高照会は 2 回 / 2 秒」のような上限がある。銘柄ごとに
残高を確認するような実装にすると即座に上限に当たるので、呼び出し側でまとめて
取得し、:class:`Cached` で短時間だけ使い回す。上限そのものは、それを知っている
:class:`~wbcore.broker.base.Broker` の実装が :class:`Limit` で宣言する。
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class Limit:
    """``per_seconds`` 秒あたり ``calls`` 回まで。"""

    calls: int
    per_seconds: float

    def __post_init__(self) -> None:
        if self.calls < 1 or self.per_seconds <= 0:
            raise ValueError(f"不正なレート制限: {self.calls}回/{self.per_seconds}秒")


class RateLimiter:
    """トークンバケット方式のレート制限。

    上限に達したら例外ではなく**待つ**。自動売買では「制限に当たったので
    発注しませんでした」より「少し待って発注する」方が望ましいため。

    スレッドセーフ。
    """

    def __init__(self, limit: Limit, *, sleep: Callable[[float], None] = time.sleep) -> None:
        self.limit = limit
        self._sleep = sleep
        self._lock = threading.Lock()
        self._allowance = float(limit.calls)
        self._last_check = time.monotonic()

    def acquire(self, tokens: float = 1.0) -> float:
        """トークンを1つ消費する。足りなければ回復するまで待つ。

        Returns:
            実際に待った秒数。
        """
        if tokens > self.limit.calls:
            raise ValueError(f"1回の要求 {tokens} が上限 {self.limit.calls} を超えています")

        with self._lock:
            waited = 0.0
            while True:
                now = time.monotonic()
                elapsed = now - self._last_check
                self._last_check = now

                # 経過時間ぶんトークンを回復させる
                self._allowance = min(
                    float(self.limit.calls),
                    self._allowance + elapsed * (self.limit.calls / self.limit.per_seconds),
                )

                if self._allowance >= tokens:
                    self._allowance -= tokens
                    return waited

                deficit = tokens - self._allowance
                delay = deficit * (self.limit.per_seconds / self.limit.calls)
                self._sleep(delay)
                waited += delay

    def __repr__(self) -> str:
        return f"<RateLimiter {self.limit.calls}回/{self.limit.per_seconds}秒>"


class Cached[T]:
    """短時間だけ結果を使い回す。

    残高や建玉のように 2 回/2 秒しか叩けないものを、1回の実行サイクル内で
    何度も参照したい場合に使う。

    Example:
        >>> balance = Cached(broker.fetch_balance, ttl=5.0)  # doctest: +SKIP
        >>> balance.get()  # 1回目は実際に取得   # doctest: +SKIP
        >>> balance.get()  # 5秒以内は同じ値を返す  # doctest: +SKIP
    """

    def __init__(
        self,
        factory: Callable[[], T],
        ttl: float = 2.0,
        *,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._factory = factory
        self._ttl = ttl
        self._clock = clock
        self._value: T | None = None
        self._fetched_at = float("-inf")
        self._lock = threading.Lock()

    def get(self) -> T:
        with self._lock:
            now = self._clock()
            if self._value is None or now - self._fetched_at >= self._ttl:
                self._value = self._factory()
                self._fetched_at = now
            return self._value

    def invalidate(self) -> None:
        """次回の取得を強制する。発注直後など、状態が変わったときに呼ぶ。"""
        with self._lock:
            self._value = None
            self._fetched_at = float("-inf")
