"""積立型戦略の実行（ドル平均法＋下降局面での増額）。

戦略そのもの（``bear_stack`` など）は :mod:`wbjp.strategy` の登録簿に
売買型と並んで登録され、``strategies.toml`` の ``[[tactics]]`` から引く。
このパッケージが持つのは、積立型に固有の**実行の仕組み**だけ。

**なぜ実行経路が売買型と別なのか**

売買型の戦略は「目標建玉」を導く意見を返し、:mod:`wbjp.portfolio.sizer`
がそれを目標株数に変換する。積立型は目標建玉を持たない——「今月いくら
**追加**するか」を決める累積の計画であり、売却も損切りもしない。無理に
Signal に載せるとサイジングが目標株数を上書きしてしまい、積立にならない。

そのため `Strategy → Sizer` ではなく `build_plan() → simulate()` の2段で
完結させている。共有するのは名前・登録・設定・一覧の面（:class:`
~wbjp.strategy.base.Playbook`）。

使い方::

    from wbjp.accumulate import build_plan, simulate, AccumulationSettings
    from wbjp.config import load_strategies

    config = load_strategies()            # config/strategies.toml
    for symbol, tactic in config.build().items():
        settings = AccumulationSettings(config.monthly_budget, tactic)
        result = simulate(bars[symbol], build_plan(bars[symbol], settings),
                          monthly_budget=config.monthly_budget)
"""

from wbjp.accumulate.basket import (
    BasketResult,
    BasketSettings,
    DrawdownTilt,
    WeightSchedule,
    build_basket_plan,
    simulate_basket,
    xirr,
)
from wbjp.accumulate.config import AccumulateConfig, BasketEntry, TacticEntry, load
from wbjp.accumulate.plan import PLAN_COLUMNS, AccumulationSettings, build_plan
from wbjp.accumulate.simulate import AccumulationResult, simulate
from wbjp.accumulate.stack import bear_stack, stack_score
from wbjp.accumulate.tactics import (
    BearStack,
    Constant,
    DrawdownLadder,
    StackLadder,
    Tactic,
)
from wbjp.accumulate.window import TradingWindow

# 登録簿は :mod:`wbjp.strategy.registry` に一本化してある。ここから再輸出すると
# registry → accumulate.tactics → accumulate/__init__ → registry で循環するので、
# 呼び出し側は wbjp.strategy.registry から直接引くこと。

__all__ = [
    "PLAN_COLUMNS",
    "AccumulateConfig",
    "AccumulationResult",
    "AccumulationSettings",
    "BasketEntry",
    "BasketResult",
    "BasketSettings",
    "BearStack",
    "Constant",
    "DrawdownLadder",
    "DrawdownTilt",
    "StackLadder",
    "Tactic",
    "TacticEntry",
    "TradingWindow",
    "WeightSchedule",
    "bear_stack",
    "build_basket_plan",
    "build_plan",
    "load",
    "simulate",
    "simulate_basket",
    "stack_score",
    "xirr",
]
