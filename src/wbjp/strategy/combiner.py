"""複数戦略のシグナルを1本に合成する。

合成方式によって性格がまるで変わる:

    ============== ==========================================================
    方式           性格
    ============== ==========================================================
    weighted_vote  重み付き平均。少数意見も一定量は反映される。既定。
    majority        過半数が同じ向きなら採用。割れたら見送る。
    veto            全員一致のときだけ動く。最も保守的で機会は減る。
    priority        優先順位の高い戦略の意見をそのまま採る。
    ============== ==========================================================

どれを選んでも、**なぜその結論になったか**を ``contributions`` に残す。
自動売買では「何を買ったか」より「なぜ買ったか」を後から追えることが重要。
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections import defaultdict
from collections.abc import Iterable, Mapping

from wbcore.domain.models import CombinedSignal, Signal


class SignalCombiner(ABC):
    """合成器の基底。"""

    #: 設定ファイルで指定する識別子。
    name: str = ""

    def __init__(self, weights: Mapping[str, float] | None = None) -> None:
        """
        Args:
            weights: 戦略名 → 重み。未指定の戦略は 1.0 とみなす。
        """
        self._weights = dict(weights or {})

    def weight(self, strategy: str) -> float:
        return self._weights.get(strategy, 1.0)

    def combine(self, signals: Iterable[Signal]) -> dict[str, CombinedSignal]:
        """銘柄ごとにまとめてから合成する。"""
        grouped: dict[str, list[Signal]] = defaultdict(list)
        for signal in signals:
            grouped[signal.symbol].append(signal)

        return {
            symbol: self._combine_symbol(symbol, symbol_signals)
            for symbol, symbol_signals in grouped.items()
        }

    @abstractmethod
    def _combine_symbol(self, symbol: str, signals: list[Signal]) -> CombinedSignal:
        """1銘柄ぶんの合成。"""

    def _contributions(self, signals: list[Signal]) -> dict[str, float]:
        return {s.strategy: s.score for s in signals}

    def __repr__(self) -> str:
        return f"<{type(self).__name__} weights={self._weights}>"


class WeightedVoteCombiner(SignalCombiner):
    """重み付き平均。既定の合成方式。

    ``direction × confidence × 戦略の重み`` の加重平均を取る。
    重みの合計で割るため、戦略の数を増やしても値域は -1〜1 に収まる。

    意見を言わなかった戦略は分母にも入らない。「黙っている」ことが
    「中立だと主張する」ことに化けないようにするため。
    """

    name = "weighted_vote"

    def _combine_symbol(self, symbol: str, signals: list[Signal]) -> CombinedSignal:
        total_weight = sum(self.weight(s.strategy) for s in signals)
        if total_weight == 0:
            return CombinedSignal(symbol, 0.0, {}, "重みの合計が0")

        weighted = sum(s.score * self.weight(s.strategy) for s in signals)
        direction = _clamp(weighted / total_weight)

        return CombinedSignal(
            symbol=symbol,
            direction=direction,
            contributions=self._contributions(signals),
            reason=f"{len(signals)}戦略の加重平均 (合計重み {total_weight:g})",
        )


class MajorityVoteCombiner(SignalCombiner):
    """多数決。

    強気・弱気の頭数を数え、過半数を取った側の平均スコアを採用する。
    同数なら中立にする（迷ったら動かない）。

    加重平均と違い、極端な意見1つに引きずられない。
    """

    name = "majority"

    #: これ未満の |score| は「意見なし」として頭数に数えない
    neutral_band: float = 0.05

    def _combine_symbol(self, symbol: str, signals: list[Signal]) -> CombinedSignal:
        bullish = [s for s in signals if s.score > self.neutral_band]
        bearish = [s for s in signals if s.score < -self.neutral_band]
        contributions = self._contributions(signals)

        if len(bullish) == len(bearish):
            return CombinedSignal(
                symbol, 0.0, contributions, f"賛否同数 (強気{len(bullish)}/弱気{len(bearish)})"
            )

        winners = bullish if len(bullish) > len(bearish) else bearish
        total_weight = sum(self.weight(s.strategy) for s in winners)
        if total_weight == 0:
            return CombinedSignal(symbol, 0.0, contributions, "重みの合計が0")

        direction = _clamp(sum(s.score * self.weight(s.strategy) for s in winners) / total_weight)
        side = "強気" if winners is bullish else "弱気"
        return CombinedSignal(
            symbol,
            direction,
            contributions,
            f"{side}多数 (強気{len(bullish)}/弱気{len(bearish)})",
        )


class VetoCombiner(SignalCombiner):
    """全員一致のときだけ動く。1つでも逆を向いたら見送る。

    最も保守的。売買機会は大きく減るが、戦略同士が食い違う
    不安定な局面を避けられる。
    """

    name = "veto"

    neutral_band: float = 0.05

    def _combine_symbol(self, symbol: str, signals: list[Signal]) -> CombinedSignal:
        contributions = self._contributions(signals)
        opinions = [s for s in signals if abs(s.score) > self.neutral_band]

        if not opinions:
            return CombinedSignal(symbol, 0.0, contributions, "意見なし")

        has_bull = any(s.score > 0 for s in opinions)
        has_bear = any(s.score < 0 for s in opinions)
        if has_bull and has_bear:
            return CombinedSignal(symbol, 0.0, contributions, "意見が割れたため見送り")

        total_weight = sum(self.weight(s.strategy) for s in opinions)
        direction = _clamp(sum(s.score * self.weight(s.strategy) for s in opinions) / total_weight)
        return CombinedSignal(symbol, direction, contributions, f"{len(opinions)}戦略が一致")


class PriorityCombiner(SignalCombiner):
    """優先順位の高い戦略の意見をそのまま採用する。

    「普段はトレンド戦略、リスクオフ検知戦略が発言したらそちらを優先」
    のような使い方を想定している。重みを優先順位として解釈する
    （重みが大きいほど優先）。
    """

    name = "priority"

    neutral_band: float = 0.05

    def _combine_symbol(self, symbol: str, signals: list[Signal]) -> CombinedSignal:
        contributions = self._contributions(signals)
        opinions = [s for s in signals if abs(s.score) > self.neutral_band]

        if not opinions:
            return CombinedSignal(symbol, 0.0, contributions, "意見なし")

        # 重みが同じ場合は戦略名で決めて、実行のたびに結果が変わらないようにする
        winner = max(opinions, key=lambda s: (self.weight(s.strategy), s.strategy))
        return CombinedSignal(
            symbol,
            _clamp(winner.score),
            contributions,
            f"最優先の {winner.strategy} を採用: {winner.reason}",
        )


COMBINERS: dict[str, type[SignalCombiner]] = {
    cls.name: cls
    for cls in (WeightedVoteCombiner, MajorityVoteCombiner, VetoCombiner, PriorityCombiner)
}


def build_combiner(name: str, weights: Mapping[str, float] | None = None) -> SignalCombiner:
    """名前から合成器を作る。

    Raises:
        ValueError: 未知の名前のとき。
    """
    try:
        combiner_cls = COMBINERS[name]
    except KeyError:
        raise ValueError(f"未知の合成方式 {name!r}。利用可能: {sorted(COMBINERS)}") from None
    return combiner_cls(weights)


def _clamp(value: float) -> float:
    """浮動小数の誤差で -1〜1 をわずかに超えるのを防ぐ。"""
    return max(-1.0, min(1.0, value))
