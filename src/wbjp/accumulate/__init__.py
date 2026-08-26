"""指数の積立（ドル平均法＋下降局面での増額）。

**なぜ Strategy ではなく独立モジュールなのか**

:mod:`wbjp.strategy` の戦略は「目標建玉」を導く意見を返し、
:mod:`wbjp.portfolio.sizer` がそれを目標株数に変換する。積立は
目標建玉を持たない——「今月いくら**追加**するか」を決める累積の
計画であり、売却も損切りもしない。無理に Signal に載せると
サイジングが目標株数を上書きしてしまい、積立にならない。

そのため独立させ、`Strategy → Sizer` ではなく
`build_plan() → simulate()` の2段で完結させている。

使い方::

    from wbjp.accumulate import load, build_plan, simulate, AccumulationSettings

    config = load()                       # config/accumulate.toml
    for symbol, tactic in config.build().items():
        settings = AccumulationSettings(config.monthly_budget, tactic)
        result = simulate(bars[symbol], build_plan(bars[symbol], settings),
                          monthly_budget=config.monthly_budget)
"""

from wbjp.accumulate.config import AccumulateConfig, TacticEntry, load
from wbjp.accumulate.plan import PLAN_COLUMNS, AccumulationSettings, build_plan
from wbjp.accumulate.registry import available, create, get, register
from wbjp.accumulate.simulate import AccumulationResult, simulate
from wbjp.accumulate.stack import bear_stack, stack_score
from wbjp.accumulate.tactics import (
    BearStack,
    Constant,
    DrawdownLadder,
    StackLadder,
    Tactic,
)

__all__ = [
    "PLAN_COLUMNS",
    "AccumulateConfig",
    "AccumulationResult",
    "AccumulationSettings",
    "BearStack",
    "Constant",
    "DrawdownLadder",
    "StackLadder",
    "Tactic",
    "TacticEntry",
    "available",
    "bear_stack",
    "build_plan",
    "create",
    "get",
    "load",
    "register",
    "simulate",
    "stack_score",
]
