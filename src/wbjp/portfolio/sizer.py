"""ポジションサイジング。合成シグナルを「目標株数」に変換する。

重要な前提:
    このシステムは **現物のみ**を扱う。空売りはしない。
    したがって弱気シグナルは「売り建て」ではなく「保有していれば手仕舞う」
    という意味になる。目標株数は常に 0 以上。

ヒステリシス:
    ``entry_threshold`` を超えたら新規に建て、``exit_threshold`` を
    下回ったら手仕舞う。2つの閾値を分けるのは、シグナルが閾値の
    周りで揺れるたびに売買を繰り返して手数料で削られるのを防ぐため。
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from decimal import Decimal

from wbcore.domain.jp_rules import DEFAULT_LOT_SIZE, round_to_lot
from wbcore.domain.models import CombinedSignal, Position, TargetPosition
from wbcore.logging import get_logger
from wbjp.config import SizingConfig

log = get_logger(__name__)


@dataclass(frozen=True, slots=True)
class SizingContext:
    """サイジングに必要な材料。

    Attributes:
        equity: 総資産（現金＋評価額）。配分の基準。
        buying_power: 買付余力。これを超える目標は作らない。
        prices: 銘柄 → 直近終値。
        atr: 銘柄 → ATR。``atr_risk`` 方式で損切り幅の見積りに使う。
        lot_sizes: 銘柄 → 売買単位の例外。
        default_lot_size: 例外の無い銘柄の売買単位。東証は100株、米国は1株。
    """

    equity: Decimal
    buying_power: Decimal
    prices: dict[str, Decimal]
    atr: dict[str, Decimal] = field(default_factory=dict)
    lot_sizes: dict[str, Decimal] = field(default_factory=dict)
    positions: dict[str, Position] = field(default_factory=dict)
    default_lot_size: Decimal = DEFAULT_LOT_SIZE

    def lot_size(self, symbol: str) -> Decimal:
        return self.lot_sizes.get(symbol, self.default_lot_size)

    def held_quantity(self, symbol: str) -> Decimal:
        position = self.positions.get(symbol)
        return position.quantity if position else Decimal(0)


class PositionSizer(ABC):
    """合成シグナル → 目標株数。"""

    name: str = ""

    def __init__(self, config: SizingConfig) -> None:
        self.config = config

    def size(
        self,
        signals: dict[str, CombinedSignal],
        ctx: SizingContext,
        *,
        entry_threshold: float,
        exit_threshold: float,
    ) -> list[TargetPosition]:
        """目標建玉を決める。

        保有中だがシグナルが届かなかった銘柄も、手仕舞い判断のために
        必ず結果に含める（そうしないと「消えた建玉」が放置される）。
        """
        targets: list[TargetPosition] = []
        candidates = self._select(signals, ctx, entry_threshold)

        for symbol in sorted(set(signals) | set(ctx.positions)):
            signal = signals.get(symbol)
            held = ctx.held_quantity(symbol)
            direction = signal.direction if signal else 0.0

            # 弱気・意見なしなら手仕舞う（空売りはできない）
            if direction < exit_threshold:
                if held > 0:
                    targets.append(
                        TargetPosition(
                            symbol,
                            Decimal(0),
                            reason=_exit_reason(signal, exit_threshold),
                        )
                    )
                continue

            # 閾値の間は現状維持（往復売買を避けるヒステリシス）
            if direction < entry_threshold:
                if held > 0:
                    targets.append(
                        TargetPosition(symbol, held, reason=f"保有継続 (強さ {direction:.2f})")
                    )
                continue

            if symbol not in candidates and held == 0:
                continue  # 保有上限に達しており、新規には建てない

            # 保有中は株数を計算し直さない。ATR や資産の変化で目標株数が
            # 日々ずれ、その差分が「意図しない部分売買」として板に出る
            # （実測: 保有継続シグナルに対する部分売却が30回/年）。
            # 建玉の増減はストップ管理（利確・手仕舞い）だけが決める。
            if held > 0:
                targets.append(
                    TargetPosition(symbol, held, reason=f"保有継続 (強さ {direction:.2f})")
                )
                continue

            quantity = self._quantity(symbol, signal, ctx)
            quantity = round_to_lot(quantity, ctx.lot_size(symbol))

            if quantity == 0 and held == 0:
                log.debug("単元株に満たないため見送り", symbol=symbol)
                continue

            targets.append(
                TargetPosition(
                    symbol,
                    max(quantity, Decimal(0)),
                    reason=f"{self.name} (強さ {direction:.2f})",
                )
            )

        return targets

    def _select(
        self,
        signals: dict[str, CombinedSignal],
        ctx: SizingContext,
        entry_threshold: float,
    ) -> set[str]:
        """保有上限の枠内に収まる新規候補を選ぶ。

        シグナルの強い順に採る。既に保有している銘柄は枠を消費する。
        """
        held = {s for s, p in ctx.positions.items() if p.quantity > 0}
        available = max(0, self.config.max_positions - len(held))

        fresh = sorted(
            (s for s, sig in signals.items() if sig.direction >= entry_threshold and s not in held),
            key=lambda s: -signals[s].direction,
        )
        return held | set(fresh[:available])

    @abstractmethod
    def _quantity(self, symbol: str, signal: CombinedSignal | None, ctx: SizingContext) -> Decimal:
        """1銘柄あたりの目標株数（単元株に丸める前）。"""

    def _price(self, symbol: str, ctx: SizingContext) -> Decimal | None:
        price = ctx.prices.get(symbol)
        return price if price and price > 0 else None


class EqualWeightSizer(PositionSizer):
    """総資産を保有上限で等分する。

    最も素直。銘柄ごとのボラティリティを考慮しないので、値動きの
    激しい銘柄がポートフォリオのリスクを支配しやすい。
    """

    name = "equal_weight"

    def _quantity(self, symbol: str, signal: CombinedSignal | None, ctx: SizingContext) -> Decimal:
        price = self._price(symbol, ctx)
        if price is None:
            return Decimal(0)
        budget = ctx.equity / Decimal(self.config.max_positions)
        # 評価額ベースの予算は、含み益があると現金を超える。最後の 1 枠が
        # 「買付余力不足」で毎回弾かれ、資金が遊んだまま止まる（実測で 7 年に
        # 900 件超）。手数料の余裕を残して買付余力に収める。
        budget = min(budget, ctx.buying_power * Decimal("0.99"))
        return max(budget, Decimal(0)) / price


class FixedNotionalSizer(PositionSizer):
    """1銘柄あたり決まった金額を投じる。

    資産規模に関係なく金額が一定なので、検証中に挙動を把握しやすい。
    """

    name = "fixed_notional"

    def _quantity(self, symbol: str, signal: CombinedSignal | None, ctx: SizingContext) -> Decimal:
        price = self._price(symbol, ctx)
        if price is None:
            return Decimal(0)
        return self.config.fixed_notional / price


class AtrRiskSizer(PositionSizer):
    """1トレードあたりの損失額を揃える（推奨）。

    考え方:
        損切り幅を ATR の ``atr_stop_multiple`` 倍に置き、そこに到達した
        場合の損失が総資産の ``risk_per_trade`` に収まる株数を求める。

            株数 = 総資産 × リスク率 ÷ (ATR × 倍率)

    値動きの激しい銘柄は自動的に少なく、穏やかな銘柄は多く持つことに
    なるため、**銘柄をまたいでリスクが揃う**。等分方式より
    ポートフォリオが安定しやすい。

    ATR が取れない銘柄は、事故を避けるため見送る（0株）。
    """

    name = "atr_risk"

    def _quantity(self, symbol: str, signal: CombinedSignal | None, ctx: SizingContext) -> Decimal:
        price = self._price(symbol, ctx)
        atr = ctx.atr.get(symbol)

        if price is None or atr is None or atr <= 0:
            log.debug("ATR が無いためサイジング不可", symbol=symbol)
            return Decimal(0)

        stop_distance = atr * self.config.atr_stop_multiple
        if stop_distance <= 0:
            return Decimal(0)

        risk_budget = ctx.equity * self.config.risk_per_trade
        quantity = risk_budget / stop_distance

        # 1銘柄が資産を食い尽くさないよう、金額でも頭を押さえる
        max_by_cash = ctx.equity / Decimal(self.config.max_positions) / price
        return min(quantity, max_by_cash)


SIZERS: dict[str, type[PositionSizer]] = {
    cls.name: cls for cls in (EqualWeightSizer, FixedNotionalSizer, AtrRiskSizer)
}


def build_sizer(config: SizingConfig) -> PositionSizer:
    """設定からサイジング方式を組み立てる。"""
    try:
        sizer_cls = SIZERS[config.method]
    except KeyError:
        raise ValueError(
            f"未知のサイジング方式 {config.method!r}。利用可能: {sorted(SIZERS)}"
        ) from None
    return sizer_cls(config)


def _exit_reason(signal: CombinedSignal | None, exit_threshold: float) -> str:
    if signal is None:
        return "シグナルが消滅したため手仕舞い"
    return f"強さ {signal.direction:.2f} が手仕舞い閾値 {exit_threshold:.2f} を下回った"
