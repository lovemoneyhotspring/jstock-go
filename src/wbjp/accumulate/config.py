"""積立の設定。実体は :mod:`wbjp.config` に統合済み。

**なぜここに実体が無いのか**

かつて積立は ``config/accumulate.toml`` と専用モデルを持っていた。
しかし売買型・積立型のどちらも取引に使う戦略であり、置き場所を分けると
片方の存在を忘れる（``bear_stack`` が戦略一覧から漏れる）。いまは
``strategies.toml`` の ``[[tactics]]`` / ``[[baskets]]`` に同居し、
:class:`wbjp.config.StrategiesConfig` が両方を持つ。

このモジュールは移行のための別名だけを提供する。新しいコードは
:mod:`wbjp.config` から直接引くこと。
"""

from __future__ import annotations

from wbjp.config import (
    STRATEGIES_FILENAME,
    BasketEntry,
    StrategiesConfig,
    TacticEntry,
    load_strategies,
)

#: 設定ファイル名。積立も売買と同じ ``strategies.toml`` に書く。
FILENAME = STRATEGIES_FILENAME

#: 統合前の名前。中身は売買型も持つ :class:`~wbjp.config.StrategiesConfig`。
AccumulateConfig = StrategiesConfig

#: 統合前の名前。
load = load_strategies

__all__ = [
    "FILENAME",
    "AccumulateConfig",
    "BasketEntry",
    "StrategiesConfig",
    "TacticEntry",
    "load",
    "load_strategies",
]
