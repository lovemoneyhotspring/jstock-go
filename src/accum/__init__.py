"""積立プロジェクト（ドル平均法＋下落局面での増額）。

スイング売買（:mod:`wbjp`）とは独立したプロジェクト。共有するのは
:mod:`wbcore` の部品——ブローカー・足データ・指標・認証情報・登録簿の
仕組み——だけで、``accum`` は ``wbjp`` を import しない。

**なぜスイング売買と分けるのか**

スイング売買の戦略は「目標建玉」を導く意見を返し、サイジングが目標株数に
変換し、差分だけを発注する。積立は目標建玉を持たない——「今月いくら
**追加**するか」を決める累積の計画で、売却も損切りもしない。同じ枠に
載せると片方の都合が片方を歪めるので、部品は共有し、方針は分ける。

部品の組み合わせ::

    足（wbcore.data） → 倍率（accum.tactics） → 計画（accum.plan）
        → 検証（accum.simulate / accum.basket）
        → 注文（accum.execute） → 発注（wbcore.broker）

どの段も単独で使える。例えば検証だけならブローカーは要らないし、
別の計画器を書けば同じ ``execute`` と ``broker`` で発注できる。

使い方::

    from accum import load, build_plan, simulate, AccumulationSettings

    config = load()                       # config/accum/accum.toml
    for symbol, tactic in config.build().items():
        settings = AccumulationSettings(config.monthly_budget, tactic)
        result = simulate(bars[symbol], build_plan(bars[symbol], settings),
                          monthly_budget=config.monthly_budget)
"""

from accum.basket import (
    BasketResult,
    BasketSettings,
    DrawdownTilt,
    WeightSchedule,
    build_basket_plan,
    simulate_basket,
    xirr,
)
from accum.config import DEFAULT_CONFIG_DIR, FILENAME, AccumConfig, BasketEntry, TacticEntry, load
from accum.plan import PLAN_COLUMNS, AccumulationSettings, build_plan
from accum.registry import TACTICS, available, create, get, register
from accum.simulate import AccumulationResult, simulate
from accum.stack import bear_stack, stack_score
from accum.tactics import BearStack, Constant, DrawdownLadder, StackLadder, Tactic
from accum.window import TradingWindow

__all__ = [
    "DEFAULT_CONFIG_DIR",
    "FILENAME",
    "PLAN_COLUMNS",
    "TACTICS",
    "AccumConfig",
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
    "available",
    "bear_stack",
    "build_basket_plan",
    "build_plan",
    "create",
    "get",
    "load",
    "register",
    "simulate",
    "simulate_basket",
    "stack_score",
    "xirr",
]
