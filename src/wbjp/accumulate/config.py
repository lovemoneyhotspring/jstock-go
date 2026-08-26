"""積立の設定。戦術と銘柄の対応を持つ。

**なぜ戦略の設定と分けるのか**

:mod:`wbjp.strategy` の設定は「銘柄ユニバースに対して全戦略が意見を出し、
合成して目標建玉を決める」形。積立は逆で、**銘柄ごとに戦術が1つ決まる**。
同じファイルに混ぜると2つの異なる意味の ``name`` が並ぶことになるため、
``config/accumulate.toml`` として独立させている。

**1銘柄に複数の戦術を割り当てるのは既定で禁止**

実運用では二重に買い付けることになり、予算が意図せず倍になる。
比較検証をしたい場合は :func:`load` に ``allow_overlap=True`` を渡すか、
:mod:`wbjp.accumulate.plan` を直接呼ぶ。
"""

from __future__ import annotations

import tomllib
from collections import defaultdict
from decimal import Decimal
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field, field_validator

from wbjp.accumulate.registry import create
from wbjp.accumulate.tactics import Tactic

#: 既定の設定ファイル名。
FILENAME = "accumulate.toml"

_RESERVED = frozenset({"id", "tactic", "symbols", "enabled"})


class TacticEntry(BaseModel):
    """1つの戦術と、それを適用する銘柄。

    Attributes:
        id: 比較表の行名になる自由なラベル。日本語でよい。
        tactic: 登録簿の鍵（``bear_stack`` など）。機構の名前。
        symbols: この戦術で積み立てる銘柄コード。
    """

    model_config = {"extra": "allow"}  # 戦術固有パラメータを受け取る

    id: str
    tactic: str
    symbols: list[str] = Field(min_length=1)
    enabled: bool = True

    @field_validator("symbols")
    @classmethod
    def _clean(cls, value: list[str]) -> list[str]:
        cleaned = [s.strip() for s in value if s.strip()]
        if not cleaned:
            raise ValueError("symbols が空です")
        duplicates = {s for s in cleaned if cleaned.count(s) > 1}
        if duplicates:
            raise ValueError(f"同じ銘柄が重複しています: {sorted(duplicates)}")
        return cleaned

    @property
    def params(self) -> dict[str, Any]:
        """戦術コンストラクタに渡す固有パラメータ。"""
        extra = self.__pydantic_extra__ or {}
        return {k: v for k, v in extra.items() if k not in _RESERVED}

    def build(self) -> Tactic:
        """戦術インスタンスを組み立てる。"""
        try:
            return create(self.tactic, self.params)
        except ValueError as exc:
            raise ValueError(f"[{self.id}] {exc}") from None


class AccumulateConfig(BaseModel):
    """``config/accumulate.toml`` の内容。"""

    model_config = {"extra": "forbid"}

    monthly_budget: Decimal = Decimal(25_000)
    """1銘柄あたりの毎月の基本予算（円）。比較の前提を揃えるため全戦術で共通。"""

    tactics: list[TacticEntry] = Field(default_factory=list)

    @property
    def active(self) -> list[TacticEntry]:
        return [t for t in self.tactics if t.enabled]

    def validate_assignment(self, *, allow_overlap: bool = False) -> None:
        """id の重複と、1銘柄への複数割り当てを検出する。

        Raises:
            ValueError: id が重複、または ``allow_overlap`` が False のときに
                同じ銘柄が複数の戦術に現れた場合。
        """
        ids = [t.id for t in self.tactics]
        dup_ids = sorted({i for i in ids if ids.count(i) > 1})
        if dup_ids:
            raise ValueError(f"id が重複しています: {dup_ids}")

        if allow_overlap:
            return
        owners: dict[str, list[str]] = defaultdict(list)
        for entry in self.active:
            for symbol in entry.symbols:
                owners[symbol].append(entry.id)
        conflicts = {s: v for s, v in owners.items() if len(v) > 1}
        if conflicts:
            detail = "、".join(f"{s} → {v}" for s, v in sorted(conflicts.items()))
            raise ValueError(
                f"1銘柄に複数の戦術が割り当てられています（二重買付になります）: {detail}"
            )

    def build(self) -> dict[str, Tactic]:
        """``銘柄 → 戦術`` に展開する。設定の主用途はこれ。"""
        return {s: entry.build() for entry in self.active for s in entry.symbols}

    def tactic_for(self, symbol: str) -> Tactic | None:
        """銘柄に割り当てられた戦術。無ければ None。"""
        return self.build().get(symbol)

    @property
    def symbols(self) -> list[str]:
        """有効な戦術が割り当てられた銘柄の一覧。"""
        return sorted({s for entry in self.active for s in entry.symbols})


def load(
    config_dir: Path | str = Path("config"), *, allow_overlap: bool = False
) -> AccumulateConfig:
    """``config/accumulate.toml`` を読む。

    Args:
        config_dir: 設定ディレクトリ。
        allow_overlap: 1銘柄に複数の戦術を許すか。比較検証のときだけ True。

    Raises:
        FileNotFoundError: 設定ファイルが無いとき。
        ValueError: 内容が不正なとき。
    """
    path = Path(config_dir) / FILENAME
    if not path.is_file():
        raise FileNotFoundError(f"積立の設定が見つかりません: {path}")
    with path.open("rb") as fh:
        raw = tomllib.load(fh)
    config = AccumulateConfig.model_validate(raw)
    config.validate_assignment(allow_overlap=allow_overlap)
    return config
