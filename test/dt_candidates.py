"""daytrade ロングの候補表を backtest のパネルキャッシュから作り直す。

根拠: vault 20-research/2026-09-jp-daytrade-ml-rerank.md「候補表の再現」
（元の /tmp/dt_rebuild.py は消えたので、Go の規則を読み直して書き直したもの。2026-09-20）

  test/.venv/bin/python test/dt_candidates.py [パネル] [出力]

規則は本番のロング脚（pkg/daytrade/universe・selection）と同じ:
  プライム・売買代金の 20 日中央値 1 億以上・時価総額 3 分位の下を除く・
  決算（前日引け後／当日）／信用規制／赤字を除く・ギャップ [-1, 0)・ストップ安でない
パネルの short_interest は公表日基準、ret1 などの lag も正しい定義なので test/dt_si_pit.py は要らない。
"""

import glob
import os
import sys

import numpy as np
import pandas as pd

MIN_TURNOVER = 1e8
VOL_FLOOR = 0.02
OUT = "test/out/dt_candidates.parquet"

# 値幅制限（pkg/wbcore/marketrules/rules.go の PriceLimitTable。基準値段がこの値「未満」なら右の幅）
LIMIT_BOUNDS = np.array([100, 200, 500, 700, 1000, 1500, 2000, 3000, 5000, 7000, 10000, 15000, 20000, 30000,
                         50000, 70000, 100000, 150000, 200000, 300000, 500000, 700000, 1000000, 1500000,
                         2000000, 3000000, 5000000, 7000000, 10000000, 15000000, 20000000, 30000000, 50000000])
LIMIT_WIDTHS = np.array([30, 50, 80, 100, 150, 300, 400, 500, 700, 1000, 1500, 3000, 4000, 5000,
                         7000, 10000, 15000, 30000, 40000, 50000, 70000, 100000, 150000, 300000,
                         400000, 500000, 700000, 1000000, 1500000, 3000000, 4000000, 5000000, 7000000, 10000000])


def limit_down(prev_close):
    width = LIMIT_WIDTHS[np.searchsorted(LIMIT_BOUNDS, prev_close, side="right")]
    return np.maximum(prev_close - width, 1.0)


def cap_tercile(g):
    """universe.CapTerciles と同じ: (時価総額, code) の昇順の順位から ceil(rank * 3 / n)。"""
    order = np.lexsort((g["code"].values, g["mkt_cap"].values))
    rank = np.empty(len(g), dtype=np.int64)
    rank[order] = np.arange(1, len(g) + 1)
    return pd.Series(np.clip(np.ceil(rank * 3 / len(g)), 1, 3), index=g.index)


def main():
    # キャッシュの名前は鍵のハッシュなので、名前順ではなく更新時刻の新しいものを使う
    panel = sys.argv[1] if len(sys.argv) > 1 else max(glob.glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    out = sys.argv[2] if len(sys.argv) > 2 else OUT
    df = pd.read_parquet(panel)
    df["d"] = pd.to_datetime(df["d"])
    df["mkt_cap"] = df["mkt_cap"].fillna(0.0)
    df = df.sort_values(["d", "code"], kind="mergesort").reset_index(drop=True)

    # 3 分位の母数は、市場区分で絞る前の「売買代金の下限を満たす全銘柄」
    base = df[df["turnover_med"] >= MIN_TURNOVER].copy()
    base["cap_tercile"] = base.groupby("d", group_keys=False)[["code", "mkt_cap"]].apply(cap_tercile)

    c = base[(base["prev_close"] > 0) & (base["segment"] == "prime") & (base["cap_tercile"] > 1)
             & ~base["earn_prev"].fillna(False) & ~base["disc_today"].fillna(False)
             & ~base["alert"].fillna(False) & ~base["is_loss"].fillna(False)].copy()
    c["gap"] = c["o"] / c["prev_close"] - 1
    c = c[(c["gap"] >= -1.0) & (c["gap"] < 0.0)]
    c = c[c["o"] > limit_down(c["prev_close"].values)]

    c["y_raw"] = c["c"] / c["o"] - 1
    c["turn_cap"] = c["turnover_med"] / c["mkt_cap"].replace(0.0, np.nan)
    # 既存規則の鍵。vol20 が無い銘柄は末尾（selection.RankKey）
    c["key_sort"] = np.where(c["vol20"].notna(),
                             np.round(c["gap"], 4) / np.maximum(c["vol20"].fillna(VOL_FLOOR), VOL_FLOOR), np.inf)
    cols = ["d", "code", "o", "c", "prev_close", "gap", "y_raw", "key_sort", "vol20", "ret1", "ret5", "ret20", "pos20",
            "prev_intraday", "turn_cap", "turnover_med", "mkt_cap", "short_interest", "earn_yield", "sector"]
    c[cols].to_parquet(out, index=False)
    per_day = c.groupby("d").size()
    print(f"{panel}\n→ {out}: {len(c):,} 行 / {len(per_day):,} 日（{c['d'].min():%Y-%m-%d}〜{c['d'].max():%Y-%m-%d}）、"
          f"1 日あたり中央値 {int(per_day.median())} 本")


if __name__ == "__main__":
    main()
