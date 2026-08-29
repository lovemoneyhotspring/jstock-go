"""積立戦略の登録簿。設定ファイルの ``tactic = "..."`` からクラスを引く。

仕組みは :class:`wbcore.registry.Registry`。スイング売買の
:mod:`wbjp.strategy.registry` と同じ部品で、名前空間だけが別。
"""

from __future__ import annotations

from accum.tactics import BearStack, Constant, DrawdownLadder, StackLadder, Tactic
from wbcore.registry import Registry

TACTICS = Registry[Tactic]("戦略")

register = TACTICS.register
available = TACTICS.available
get = TACTICS.get
create = TACTICS.create

for _cls in (Constant, BearStack, StackLadder, DrawdownLadder):
    register(_cls)
