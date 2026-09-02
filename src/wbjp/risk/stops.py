"""損切りの管理（エンジン合成）。

逆指値はブローカーに置かない。ストップ価格をローカル（``stops`` テーブル）に
保持し、足が更新されるたびに評価して、抵触していれば決済注文を組み立てる。

日足で運用する以上、判定は1日1回しかできない。場中に急落しても
翌営業日の寄付で手仕舞うことになり、想定より大きく下で約定しうる。
「逆指値を使わない」×「日足で運用する」から必然的に生じる制約で、
許容できないなら分足運用とリアルタイムデータが要る。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, replace
from decimal import Decimal

from wbcore.domain.jp_rules import round_to_lot
from wbcore.domain.models import Position, TargetPosition
from wbcore.logging import get_logger

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class Stop:
    """1建玉ぶんのストップ。

    Attributes:
        stop_price: この価格以下になったら手仕舞う。
        trailing: True ならトレーリングストップ（有利な方向にのみ動く）。
        atr_multiple: ATR の何倍を損切り幅としたか（更新時に使う）。
        highest_close: トレーリング用に記録する、建てて以降の最高終値。
    """

    symbol: str
    stop_price: Decimal
    entry_price: Decimal
    created_on: dt.date
    trailing: bool = False
    atr_multiple: Decimal = Decimal("2.0")
    #: 設定されていれば ATR ではなく「最高終値 × (1 − 比率)」で追従する（%トレーリング）
    trailing_pct: Decimal | None = None
    highest_close: Decimal | None = None
    #: 設定時の初期ストップ。1R（初期リスク）の基準として、ストップを
    #: 動かした後も変わらない。None は旧レコード（stop_price を初期値とみなす）。
    initial_stop_price: Decimal | None = None
    #: 設定時の建玉数。2段階利確で「元の何%を売ったか」を計算する基準。
    #: None は旧レコード（利確前に作られた・利確機能を使わない）。
    initial_quantity: Decimal | None = None
    #: 1段目の利確（部分手仕舞い）が済んだか。済んだ後は
    #: :meth:`StopBook.trend_exit_targets` が残り玉の手仕舞いを判定する。
    scaled_out: bool = False

    def is_triggered(self, price: Decimal) -> bool:
        return price <= self.stop_price

    @property
    def risk_per_share(self) -> Decimal:
        """現在のストップまでの距離。"""
        return self.entry_price - self.stop_price

    @property
    def initial_risk(self) -> Decimal:
        """1R = 建値 − 初期ストップ。利確目標・建値移動の基準。"""
        return self.entry_price - (self.initial_stop_price or self.stop_price)

    def trading_days_held(self, as_of: dt.date) -> int:
        """建ててからの営業日数（土日を除く。祝日は数える）。"""
        if as_of <= self.created_on:
            return 0
        days = 0
        current = self.created_on
        while current < as_of:
            current += dt.timedelta(days=1)
            if current.weekday() < 5:
                days += 1
        return days


class StopBook:
    """建玉ごとのストップを管理する。

    永続化は :mod:`wbjp.db` が担当する。ここは判定のみ。
    """

    def __init__(self, stops: dict[str, Stop] | None = None) -> None:
        self._stops: dict[str, Stop] = dict(stops or {})

    def __contains__(self, symbol: str) -> bool:
        return symbol in self._stops

    def __len__(self) -> int:
        return len(self._stops)

    def get(self, symbol: str) -> Stop | None:
        return self._stops.get(symbol)

    def all(self) -> dict[str, Stop]:
        return dict(self._stops)

    def set(self, stop: Stop) -> None:
        self._stops[stop.symbol] = stop

    def remove(self, symbol: str) -> None:
        self._stops.pop(symbol, None)

    # -- 管理 ---------------------------------------------------------------

    def ensure(
        self,
        positions: dict[str, Position],
        atr: dict[str, Decimal],
        today: dt.date,
        *,
        atr_multiple: Decimal = Decimal("2.0"),
        trailing: bool = False,
        initial_stop_pct: Decimal | None = None,
        trailing_atr_multiple: Decimal | None = None,
        trailing_pct: Decimal | None = None,
    ) -> None:
        """建玉に対してストップを用意し、消えた建玉のぶんを片付ける。

        ストップを持たない建玉があるのは危険な状態なので、
        取得価格から自動で設定する。

        Args:
            initial_stop_pct: 建値からの比率でストップ幅を固定する
                （例: ``Decimal("0.04")`` = -4%）。指定すれば ATR は使わない。
                None なら従来通り ATR × ``atr_multiple`` を幅にする。
            trailing_atr_multiple: トレーリング時の幅（ATR 倍率）。None なら
                ``atr_multiple`` と同じ。
        """
        for symbol, position in positions.items():
            if position.quantity <= 0 or symbol in self._stops:
                continue

            if initial_stop_pct is not None:
                distance = position.cost_price * initial_stop_pct
            else:
                atr_value = atr.get(symbol)
                if atr_value is None or atr_value <= 0:
                    log.warning("ATR が無いためストップを設定できません", symbol=symbol)
                    continue
                distance = atr_value * atr_multiple

            stop_price = position.cost_price - distance
            if stop_price <= 0:
                continue

            self.set(
                Stop(
                    symbol=symbol,
                    stop_price=stop_price,
                    entry_price=position.cost_price,
                    created_on=today,
                    trailing=trailing,
                    atr_multiple=trailing_atr_multiple or atr_multiple,
                    trailing_pct=trailing_pct,
                    highest_close=position.last_price,
                    initial_stop_price=stop_price,
                    initial_quantity=position.quantity,
                )
            )
            log.info("ストップを設定", symbol=symbol, stop_price=str(stop_price))

        # 建玉が無くなった銘柄のストップは残さない
        for symbol in list(self._stops):
            existing = positions.get(symbol)
            if existing is None or existing.quantity <= 0:
                self.remove(symbol)

    def update_trailing(self, closes: dict[str, Decimal], atr: dict[str, Decimal]) -> None:
        """トレーリングストップを引き上げる。

        **下げることは絶対にしない。** 下げてしまうと損失が青天井になる。
        """
        for symbol, stop in list(self._stops.items()):
            if not stop.trailing:
                continue

            close = closes.get(symbol)
            if close is None:
                continue
            highest = max(stop.highest_close or close, close)
            if stop.trailing_pct is not None:
                candidate = highest * (1 - stop.trailing_pct)
            else:
                distance = atr.get(symbol)
                if distance is None or distance <= 0:
                    continue
                candidate = highest - distance * stop.atr_multiple

            if candidate > stop.stop_price:
                self._stops[symbol] = replace(stop, stop_price=candidate, highest_close=highest)
                log.info(
                    "トレーリングストップを引き上げ",
                    symbol=symbol,
                    before=str(stop.stop_price),
                    after=str(candidate),
                )
            elif highest != stop.highest_close:
                self._stops[symbol] = replace(stop, highest_close=highest)

    def update_breakeven(self, closes: dict[str, Decimal], after_r: Decimal | None) -> None:
        """含み益が ``after_r`` R に達した建玉のストップを建値に引き上げる。

        「勝ちトレードを負けに変えない」ための一手。建値より上には
        動かさない（それはトレーリングの仕事）。下げることは絶対にしない。
        """
        if after_r is None:
            return
        for symbol, stop in list(self._stops.items()):
            close = closes.get(symbol)
            if close is None or stop.stop_price >= stop.entry_price:
                continue
            risk = stop.initial_risk
            if risk <= 0:
                continue
            highest = max(stop.highest_close or close, close)
            if (highest - stop.entry_price) / risk >= after_r:
                self._stops[symbol] = replace(
                    stop, stop_price=stop.entry_price, highest_close=highest
                )
                log.info(
                    "ストップを建値に引き上げ",
                    symbol=symbol,
                    entry=str(stop.entry_price),
                    before=str(stop.stop_price),
                )

    # -- 判定 ---------------------------------------------------------------

    def time_exit_targets(
        self,
        closes: dict[str, Decimal],
        as_of: dt.date,
        *,
        stale_days: int | None = None,
        max_days: int | None = None,
    ) -> list[TargetPosition]:
        """時間切れの手仕舞い目標。

        - ``stale_days`` 営業日たっても含み益ゼロ以下 → 前提が崩れている
        - ``max_days`` 営業日 → 資金効率のため強制決済
        """
        targets = []
        for symbol, stop in self._stops.items():
            close = closes.get(symbol)
            if close is None:
                continue
            held = stop.trading_days_held(as_of)
            reason = None
            if max_days is not None and held >= max_days:
                reason = f"最大保有期間 {max_days} 営業日に到達"
            elif stale_days is not None and held >= stale_days and close <= stop.entry_price:
                reason = (
                    f"{held} 営業日たっても含み益なし（終値 {close} ≦ 建値 {stop.entry_price}）"
                )
            if reason:
                log.info("時間切れで手仕舞い", symbol=symbol, reason=reason)
                targets.append(TargetPosition(symbol, Decimal(0), reason=reason))
        return targets

    def triggered(self, closes: dict[str, Decimal]) -> dict[str, Stop]:
        """抵触したストップを返す。"""
        return {
            symbol: stop
            for symbol, stop in self._stops.items()
            if (close := closes.get(symbol)) is not None and stop.is_triggered(close)
        }

    def exit_targets(self, closes: dict[str, Decimal]) -> list[TargetPosition]:
        """抵触した建玉の手仕舞い目標（0株）を作る。

        この目標は戦略のシグナルより**優先**される。損切りは
        戦略の意見に関係なく実行しなければ意味がない。
        """
        targets = []
        for symbol, stop in self.triggered(closes).items():
            close = closes[symbol]
            log.warning(
                "ストップに抵触",
                symbol=symbol,
                close=str(close),
                stop_price=str(stop.stop_price),
            )
            targets.append(
                TargetPosition(
                    symbol,
                    Decimal(0),
                    reason=(
                        f"ストップ抵触: 終値 {close} ≦ ストップ {stop.stop_price}"
                        "（日足のため翌営業日の寄付で決済）"
                    ),
                )
            )
        return targets

    def take_profit_targets(
        self,
        closes: dict[str, Decimal],
        quantities: dict[str, Decimal],
        lot_sizes: dict[str, Decimal],
        *,
        target_r: Decimal | None,
        fraction: Decimal = Decimal("0.5"),
        default_lot_size: Decimal = Decimal(1),
    ) -> list[TargetPosition]:
        """2段階利確の1段目: 含み益が ``target_r`` に達した建玉の一部を利確する。

        まだ利確していない建玉だけが対象（``scaled_out`` で一度きり）。
        利確と同時にストップを建値へ引き上げる（下げることはしない）。
        残りの手仕舞いは :meth:`trend_exit_targets` に委ねる。
        """
        if target_r is None:
            return []

        targets = []
        for symbol, stop in list(self._stops.items()):
            if stop.scaled_out or stop.initial_quantity is None:
                continue
            close = closes.get(symbol)
            current_qty = quantities.get(symbol)
            if close is None or current_qty is None or current_qty <= 0:
                continue
            risk = stop.initial_risk
            if risk <= 0:
                continue
            gain_r = (close - stop.entry_price) / risk
            if gain_r < target_r:
                continue

            lot = lot_sizes.get(symbol, default_lot_size)
            remaining = round_to_lot(stop.initial_quantity * (Decimal(1) - fraction), lot)
            remaining = min(remaining, current_qty)

            self._stops[symbol] = replace(
                stop, scaled_out=True, stop_price=max(stop.stop_price, stop.entry_price)
            )
            log.info(
                "利確: 建玉の一部を手仕舞い",
                symbol=symbol,
                gain_r=str(gain_r),
                remaining=str(remaining),
            )
            if remaining < current_qty:
                targets.append(
                    TargetPosition(
                        symbol,
                        remaining,
                        reason=(
                            f"利確 +{gain_r:.1f}R 到達、{fraction:.0%} を手仕舞い"
                            "（残りはトレンド追従）"
                        ),
                    )
                )
        return targets

    def runner_targets(
        self,
        closes: dict[str, Decimal],
        quantities: dict[str, Decimal],
        trend_values: dict[str, Decimal],
        *,
        always: bool = False,
    ) -> list[TargetPosition]:
        """2段階利確の2段目: 利確済みの建玉を、トレンド追従で保有し続ける。

        ``always=True`` なら利確前の建玉にも適用し、移動平均割れそのものを
        利確・損切りの合図にする（短い移動平均で頂点近くで降りる検証用）。

        対象は :meth:`take_profit_targets` で既に一部を利確した建玉だけ。
        利確前の建玉は初期ストップ・建値ストップ・時間切れが担当する
        （押し目そのものが移動平均近辺での下落なので、利確前に当てると
        エントリー直後に手仕舞ってしまう）。

        **なぜ「保有継続」もここで明示するのか**
            サイジング（``atr_risk`` など）は毎日、資産と ATR から目標株数を
            計算し直す。何も言わないと、利確で減らした株数が翌日には
            満額へ買い戻されてしまう。ここで残数をストップ優先の目標として
            固定し、移動平均を割ったときだけ手仕舞いに切り替える。
        """
        targets = []
        for symbol, stop in self._stops.items():
            if not stop.scaled_out and not always:
                continue
            close = closes.get(symbol)
            current_qty = quantities.get(symbol)
            if close is None or current_qty is None or current_qty <= 0:
                continue

            trend = trend_values.get(symbol)
            if trend is not None and close < trend:
                log.info(
                    "トレンド割れで残りを手仕舞い",
                    symbol=symbol,
                    close=str(close),
                    trend=str(trend),
                )
                targets.append(
                    TargetPosition(
                        symbol,
                        Decimal(0),
                        reason=f"終値 {close} が移動平均 {trend} を割り込み、残りを手仕舞い",
                    )
                )
            else:
                targets.append(
                    TargetPosition(
                        symbol, current_qty, reason="利確後の残り玉を維持（トレンド追従中）"
                    )
                )
        return targets


def apply_stop_priority(
    strategy_targets: list[TargetPosition],
    stop_targets: list[TargetPosition],
) -> list[TargetPosition]:
    """ストップによる手仕舞いを、戦略の目標より優先して重ねる。

    同じ銘柄について戦略が「買い増し」と言っていても、ストップに
    抵触していれば手仕舞いが勝つ。
    """
    merged = {t.symbol: t for t in strategy_targets}
    for target in stop_targets:
        merged[target.symbol] = target
    return sorted(merged.values(), key=lambda t: t.symbol)
