"""仮想通貨（BTC）が大きく動いた日に daytrade ロングの成績が変わるかを測る（日の条件の検定）。

根拠: vault 20-research/2026-09-jp-daytrade-btc-days.md

  uv run --with yfinance test/dt_btc_days.py --fetch   # BTC-USD の日足を test/out/btc_usd_1d.csv に
  test/.venv/bin/python test/dt_btc_days.py            # 検定

選定は本番のロング（gap_vol の上位 3・業種 1 銘柄まで・1 単元が予算内・逆ボラ配分・流動性別コスト）。
12 月は本番が休むので除く。日の条件なので日次に畳んでから Welch（シグナル単位だと t が膨らむ）。

時点: Yahoo の BTC-USD の日足は UTC の 0 時区切り＝日本時間 9:00 区切り。日付 t の足の終値は
日本時間 t+1 日 9:00 の値なので、日本の営業日 d に使えるのは「d-1 日付の足の終値」まで（寄りの瞬間）。
"""

import argparse
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, "test")

BTC = "test/out/btc_usd_1d.csv"


def fetch():
    import yfinance as yf
    h = yf.Ticker("BTC-USD").history(period="max", interval="1d", auto_adjust=False)
    h.index = h.index.tz_localize(None).normalize()
    h[["Close"]].rename(columns={"Close": "close"}).rename_axis("utc_date").to_csv(BTC)
    print(f"{BTC}: {len(h)} 行 {h.index.min().date()}〜{h.index.max().date()}")


def daily_long():
    from dt_wf_target import CAND, VOL_FLOOR, liq_cost_bp, pick  # lightgbm を読むので --fetch では読まない

    df = pd.read_parquet(CAND)
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    df = df.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    idx = [i for _, g in df.groupby("d", sort=False) for i in pick(g.head(60))]
    p = df.loc[idx].copy()
    inv = 1.0 / np.maximum(p["vol20"].fillna(VOL_FLOOR), VOL_FLOOR)
    p["w"] = inv / inv.groupby(p["d"]).transform("sum")
    p["net"] = p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4)
    day = p.groupby("d").agg(bp=("net", "sum"), n=("net", "size"))
    day["bp"] *= 1e4
    return day[day.index.month != 12]


def btc_features(days):
    b = pd.read_csv(BTC, parse_dates=["utc_date"]).set_index("utc_date")["close"]
    b = b[~b.index.duplicated()].asfreq("D").ffill()
    out = pd.DataFrame(index=days)
    last = days - pd.Timedelta(days=1)  # 寄りの時点で確定している最後の足
    for k in (1, 5, 20):
        r = b / b.shift(k) - 1
        out[f"btc{k}"] = r.reindex(last).values
    return out


def welch(a, b):
    ma, mb = a.mean(), b.mean()
    se = np.sqrt(a.var(ddof=1) / len(a) + b.var(ddof=1) / len(b))
    return ma, mb, ma - mb, (ma - mb) / se


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fetch", action="store_true")
    a = ap.parse_args()
    if a.fetch:
        fetch()
        return

    day = daily_long()
    d = day.join(btc_features(day.index)).dropna()
    print(f"期間 {d.index.min().date()}〜{d.index.max().date()}  日数 {len(d)}  全体 {d.bp.mean():+.2f} bp/日\n")

    # 1. 相関（日次）
    print("| 窓 | 日次 bp との相関 | t |")
    print("|---|---|---|")
    for k in (1, 5, 20):
        r = d["bp"].corr(d[f"btc{k}"])
        t = r * np.sqrt((len(d) - 2) / (1 - r * r))
        print(f"| {k} 日 | {r:+.3f} | {t:+.2f} |")

    # 2. 上位・下位 20% の日 対 それ以外（窓 3 × 向き 2 = 6 検定。Bonferroni で |t| 2.64）
    print("\n| 窓 | 帯 | BTC の変化（帯の中央値） | 日数 | 帯の bp/日 | それ以外 | 差 | t |")
    print("|---|---|---|---|---|---|---|---|")
    for k in (1, 5, 20):
        x = d[f"btc{k}"]
        for name, m in (("上位 20%", x >= x.quantile(0.8)), ("下位 20%", x <= x.quantile(0.2))):
            ma, mb, diff, t = welch(d.bp[m], d.bp[~m])
            print(f"| {k} 日 | {name} | {x[m].median():+.1%} | {m.sum()} | {ma:+.2f} | {mb:+.2f} | {diff:+.2f} | {t:+.2f} |")

    # 3. 5 日の変化の 5 分位
    print("\n| 5 日の変化の 5 分位 | 範囲 | 日数 | bp/日 | 勝ち日の割合 |")
    print("|---|---|---|---|---|")
    d["q5"] = pd.qcut(d["btc5"], 5, labels=False) + 1
    for q, g in d.groupby("q5"):
        print(f"| {q} | {g.btc5.min():+.1%}〜{g.btc5.max():+.1%} | {len(g)} | {g.bp.mean():+.2f} | {(g.bp > 0).mean():.0%} |")

    # 4. いまの位置（最新の足）
    b = pd.read_csv(BTC, parse_dates=["utc_date"]).set_index("utc_date")["close"]
    print(f"\n最新の足 {b.index[-1].date()} 終値 {b.iloc[-1]:,.0f}  1 日 {b.iloc[-1] / b.iloc[-2] - 1:+.1%}  "
          f"5 日 {b.iloc[-1] / b.iloc[-6] - 1:+.1%}  20 日 {b.iloc[-1] / b.iloc[-21] - 1:+.1%}")
    for k, v in ((5, b.iloc[-1] / b.iloc[-6] - 1), (20, b.iloc[-1] / b.iloc[-21] - 1)):
        print(f"  {k} 日の変化は過去の {(d[f'btc{k}'] < v).mean():.0%} 点")


if __name__ == "__main__":
    main()
