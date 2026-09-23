"""仮想通貨（BTC）が大きく動いた日に daytrade ロングの成績が変わるかを測る（日の条件の検定）。

根拠: vault 20-research/2026-09-jp-daytrade-btc-days.md

  uv run --with yfinance test/dt_btc_days.py --fetch   # BTC-USD の日足を test/out/btc_usd_1d.csv に
  test/.venv/bin/python test/dt_btc_days.py            # 検定
  test/.venv/bin/python test/dt_btc_days.py --since 2025-09-01   # 直近だけ（トレジャリー銘柄が出てきた後）
  test/.venv/bin/python test/dt_btc_days.py --since 2024-06-01 --add 33500   # メタプラネットを母集団に入れたら

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
# プライムの仮想通貨関連（交換所・事業あり）。トレジャリー銘柄（メタプラネットなど）はスタンダード・グロースで母集団の外
CRYPTO = {"84730": "SBI HD", "86980": "マネックス", "94490": "GMO インターネット", "36960": "セレス",
          "33500": "メタプラネット（--add のときだけ候補に入る）"}


def fetch():
    import yfinance as yf
    h = yf.Ticker("BTC-USD").history(period="max", interval="1d", auto_adjust=False)
    h.index = h.index.tz_localize(None).normalize()
    h[["Close"]].rename(columns={"Close": "close"}).rename_axis("utc_date").to_csv(BTC)
    print(f"{BTC}: {len(h)} 行 {h.index.min().date()}〜{h.index.max().date()}")


def extra_rows(codes):
    """市場区分と赤字の除外だけを外して、指定の銘柄を候補表と同じ形で作る（ほかの除外は本番と同じ）。"""
    import glob
    import os

    from dt_candidates import MIN_TURNOVER, VOL_FLOOR, limit_down
    panel = max(glob.glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    df = pd.read_parquet(panel, columns=["d", "code", "o", "c", "prev_close", "vol20", "sector", "turnover_med",
                                         "mkt_cap", "earn_prev", "disc_today", "alert"])
    df["d"] = pd.to_datetime(df["d"])
    base = df[df["turnover_med"] >= MIN_TURNOVER].copy()
    base["mkt_cap"] = base["mkt_cap"].fillna(0.0)
    base["tercile"] = np.ceil(base.groupby("d")["mkt_cap"].rank(method="first") * 3
                              / base.groupby("d")["mkt_cap"].transform("size")).clip(1, 3)
    c = base[base["code"].isin(codes) & (base["prev_close"] > 0) & (base["tercile"] > 1)
             & ~base["earn_prev"].fillna(False) & ~base["disc_today"].fillna(False) & ~base["alert"].fillna(False)].copy()
    c["gap"] = c["o"] / c["prev_close"] - 1
    c = c[(c["gap"] >= -1.0) & (c["gap"] < 0) & (c["o"] > limit_down(c["prev_close"].values))]
    c["y_raw"] = c["c"] / c["o"] - 1
    c["key_sort"] = np.where(c["vol20"].notna(),
                             np.round(c["gap"], 4) / np.maximum(c["vol20"].fillna(VOL_FLOOR), VOL_FLOOR), np.inf)
    return c


def daily_long(add=()):
    from dt_wf_target import CAND, VOL_FLOOR, liq_cost_bp, pick  # lightgbm を読むので --fetch では読まない

    df = pd.read_parquet(CAND)
    if add:
        x = extra_rows(list(add))
        df = pd.concat([df, x[[k for k in df.columns if k in x.columns]]], ignore_index=True)
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    df = df.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    idx = [i for _, g in df.groupby("d", sort=False) for i in pick(g.head(60))]
    p = df.loc[idx].copy()
    inv = 1.0 / np.maximum(p["vol20"].fillna(VOL_FLOOR), VOL_FLOOR)
    p["w"] = inv / inv.groupby(p["d"]).transform("sum")
    p["net"] = p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4)
    p = p[p["d"].dt.month != 12]
    day = p.groupby("d").agg(bp=("net", "sum"), n=("net", "size"))
    day["bp"] *= 1e4
    return day, p


def btc_features(days):
    b = pd.read_csv(BTC, parse_dates=["utc_date"]).set_index("utc_date")["close"]
    b = b[~b.index.duplicated()].asfreq("D").ffill()
    out = pd.DataFrame(index=days)
    last = days - pd.Timedelta(days=1)  # 寄りの時点で確定している最後の足
    for k in (1, 5, 20):
        r = b / b.shift(k) - 1
        out[f"btc{k}"] = r.reindex(last).values
    return out


def liq_cost_bp_(turnover):
    from dt_wf_target import liq_cost_bp
    return liq_cost_bp(turnover)


def welch(a, b):
    ma, mb = a.mean(), b.mean()
    se = np.sqrt(a.var(ddof=1) / len(a) + b.var(ddof=1) / len(b))
    return ma, mb, ma - mb, (ma - mb) / se


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fetch", action="store_true")
    ap.add_argument("--since", default="", help="この日以降だけで測る（YYYY-MM-DD）")
    ap.add_argument("--add", default="", help="市場区分・赤字の除外を外して候補に混ぜる銘柄（例 33500）")
    a = ap.parse_args()
    if a.fetch:
        fetch()
        return

    add = [k for k in a.add.split(",") if k]
    day, picks = daily_long(add)
    if a.since:
        day, picks = day[day.index >= a.since], picks[picks["d"] >= a.since]
    if add:
        base, _ = daily_long()
        both = day.join(base["bp"].rename("base"), how="inner")
        diff = both["bp"] - both["base"]
        ch = diff[diff.abs() > 1e-9]
        print(f"{','.join(add)} を入れた場合: {both.bp.mean():+.2f} bp/日（入れない {both.base.mean():+.2f}）"
              f"差 {diff.mean():+.2f} bp/日、選定が変わった日 {len(ch)} 日（その日の差の平均 {ch.mean():+.1f} bp、"
              f"t {ch.mean() / (ch.std(ddof=1) / np.sqrt(len(ch))) if len(ch) > 1 else float('nan'):+.2f}）\n")
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

    # 4. 仮想通貨関連の銘柄が上位 3 に入った日
    c = picks[picks["code"].isin(CRYPTO)].copy()
    c["bp"] = (c["y_raw"] - liq_cost_bp_(c["turnover_med"].values) / 1e4) * 1e4
    print(f"\n仮想通貨関連が上位 3 に入った: {len(c)} 件 / {picks['d'].nunique()} 日"
          f"（銘柄の平均 {c.bp.mean():+.1f} bp、上位 3 全体の銘柄平均 "
          f"{((picks.y_raw - liq_cost_bp_(picks.turnover_med.values) / 1e4) * 1e4).mean():+.1f} bp）")
    for r in c.sort_values("d").tail(10).itertuples():
        print(f"  {r.d.date()} {CRYPTO[r.code]} ギャップ {r.gap:+.1%} 寄り→引け {r.y_raw:+.1%}")

    # 5. いまの位置（最新の足）
    b = pd.read_csv(BTC, parse_dates=["utc_date"]).set_index("utc_date")["close"]
    print(f"\n最新の足 {b.index[-1].date()} 終値 {b.iloc[-1]:,.0f}  1 日 {b.iloc[-1] / b.iloc[-2] - 1:+.1%}  "
          f"5 日 {b.iloc[-1] / b.iloc[-6] - 1:+.1%}  20 日 {b.iloc[-1] / b.iloc[-21] - 1:+.1%}")
    for k, v in ((5, b.iloc[-1] / b.iloc[-6] - 1), (20, b.iloc[-1] / b.iloc[-21] - 1)):
        print(f"  {k} 日の変化は過去の {(d[f'btc{k}'] < v).mean():.0%} 点")


if __name__ == "__main__":
    main()
