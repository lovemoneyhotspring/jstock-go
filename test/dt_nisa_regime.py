"""新 NISA（2024-01〜）の前後で、デイトレの利益源（ギャップダウンで寄った銘柄の寄→引の戻り）が変わったか。

根拠: vault 20-research/2026-09-jp-daytrade-nisa-regime.md（2026-09-26、ユーザの問い。採否を決める検証ではなく記述）

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  test/.venv/bin/python test/dt_nisa_regime.py            # NISA の前後
  test/.venv/bin/python test/dt_nisa_regime.py --rough    # 荒れた日の中身（市場か個別か・売買代金・ギャップの分解）

見るもの（日ごとに畳んでから年・前後で平均。t は前後の日の平均の差を独立 2 標本で）:
  GD超過    ギャップ −1% 以下の銘柄の寄→引から、その日の候補全体の平均を引いたもの（bp）
  上位10超過 gap_vol（key_sort）の上位 10 本の同じ超過（本番の並べ方の素の効き。コスト・誤差・業種の上限なし）
  反転傾き  ギャップ（%pt）に対する寄→引（bp）の日ごとの傾き（負ほど深く下げた銘柄ほど戻る）
  GD2割合   ギャップ −2% 以下の銘柄の割合（%、地合いの荒れ）
"""

import sys

import numpy as np
import pandas as pd

CAND = "test/out/dt_candidates_wide.parquet"
NISA = pd.Timestamp("2024-01-01")


def daily():
    df = pd.read_parquet(CAND, columns=["d", "gap", "y_raw", "key_sort"])
    df = df[(df["d"] >= "2017-01-01") & df["y_raw"].notna()]
    df["ex"] = df["y_raw"] - df.groupby("d")["y_raw"].transform("mean")
    gd = df[df["gap"] <= -0.01]
    top = df[np.isfinite(df["key_sort"])].sort_values(["d", "key_sort"]).groupby("d").head(10)

    def slope(g):
        return np.polyfit(g["gap"] * 100, g["y_raw"] * 1e4, 1)[0] if len(g) > 50 else np.nan

    return pd.DataFrame({
        "mkt": df.groupby("d")["y_raw"].mean() * 1e4,
        "gd_ex": gd.groupby("d")["ex"].mean() * 1e4,
        "top10_ex": top.groupby("d")["ex"].mean() * 1e4,
        "slope": df[df["gap"] < 0].groupby("d")[["gap", "y_raw"]].apply(slope),
        "share2": (df["gap"] <= -0.02).groupby(df["d"]).mean() * 100,
    })


def diff_t(a, b):
    a, b = a.dropna(), b.dropna()
    d = b.mean() - a.mean()
    return a.mean(), b.mean(), d, d / np.sqrt(a.var() / len(a) + b.var() / len(b))


def main():
    day = daily()
    t = day.groupby(day.index.year).agg(日=("mkt", "size"), 寄引_市場=("mkt", "mean"), GD超過=("gd_ex", "mean"),
                                        上位10超過=("top10_ex", "mean"), 反転傾き=("slope", "mean"), GD2割合=("share2", "mean"))
    print(t.round(1).to_string())
    pre, post = day[day.index < NISA], day[day.index >= NISA]
    mid = day[(day.index >= NISA) & (day.index < "2026-01-01")]
    print("\n前（2017〜2023）と後（2024〜）")
    for c in ("gd_ex", "top10_ex", "slope", "share2"):
        a, b, d, tt = diff_t(pre[c], post[c])
        print(f"  {c}: 前 {a:+.1f} 後 {b:+.1f} 差 {d:+.1f}（t {tt:+.2f}）")
    a, b, d, tt = diff_t(pre["top10_ex"], mid["top10_ex"])
    print(f"  top10_ex（後を 2024〜2025 に絞る）: 差 {d:+.1f}（t {tt:+.2f}）")
    day["荒れ"] = pd.qcut(day["share2"].rank(method="first"), 4, labels=["静", "やや静", "やや荒", "荒"])
    day["後"] = day.index >= NISA
    print("\n地合いの荒れ（GD2割合の 4 分位）ごとの上位10超過（bp）")
    print(day.groupby(["荒れ", "後"], observed=True)["top10_ex"].agg(["mean", "size"]).round(1).unstack().to_string())


def rough():
    """荒れた日（ギャップ −2% 以下の割合の 4 分位）の戻りの中身。2026-09-26 追記"""
    df = pd.read_parquet(CAND, columns=["d", "gap", "y_raw", "key_sort", "turnover_med"])
    df = df[(df["d"] >= "2017-01-01") & df["y_raw"].notna()].copy()
    df["mg"] = df.groupby("d")["gap"].transform("median")      # 市場のギャップ
    df["ig"] = df["gap"] - df["mg"]                              # 銘柄だけのギャップ
    df["ex"] = df["y_raw"] - df.groupby("d")["y_raw"].transform("mean")
    s2 = (df["gap"] <= -0.02).groupby(df["d"]).mean()
    q = pd.qcut(s2.rank(method="first"), 4, labels=["静", "やや静", "やや荒", "荒"])
    df["q"] = df["d"].map(q)
    day = df.groupby("d").agg(mkt=("y_raw", "mean"), mg=("mg", "first"), q=("q", "first"))
    print("1. 市場全体（日平均、bp）: 市場のギャップ mg と寄→引 mkt")
    print((day.groupby("q", observed=True)[["mg", "mkt"]].mean() * 1e4).round(1).to_string())
    for lab in ("静", "荒"):
        m = day["q"] == lab
        print(f"  {lab}: 市場のギャップ 1%pt あたりの市場の寄→引 {np.polyfit(day.mg[m] * 100, day.mkt[m] * 1e4, 1)[0]:+.1f} bp")
    top = df[np.isfinite(df["key_sort"])].sort_values(["d", "key_sort"]).groupby("d").head(10).copy()
    top["tq"] = pd.cut(np.log10(top["turnover_med"]), [0, 9, 10, 20], labels=["〜10億", "10〜100億", "100億〜"])
    print("\n2. gap_vol 上位 10 本の超過（bp）を売買代金（20 日中央値）別に")
    print((top.groupby(["q", "tq"], observed=True)["ex"].mean() * 1e4).round(1).unstack().to_string())
    print(top.groupby(["q", "tq"], observed=True).size().unstack().to_string())

    def coef(g):
        g = g[g["gap"] < 0]
        return np.polyfit(g["ig"] * 100, g["y_raw"] * 1e4, 1)[0] if len(g) >= 50 else np.nan

    b = pd.DataFrame({"b": df.groupby("d")[["gap", "ig", "y_raw"]].apply(coef), "q": q})
    print("\n3. 銘柄だけのギャップ 1%pt あたりの寄→引（bp、負ほど戻る）")
    print(b.groupby("q", observed=True)["b"].mean().round(1).to_string())


if __name__ == "__main__":
    rough() if "--rough" in sys.argv else main()
