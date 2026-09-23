"""メタプラネット（3350）が下に窓を開けた日の寄り→引けを、BTC の前日の騰落・BTC の価格水準・株価帯で分ける。

根拠: vault 20-research/2026-09-jp-daytrade-btc-days.md「追記: メタプラネットの寄り→引けは何で決まるか」

  test/.venv/bin/python test/dt_metaplanet_btc.py [--code 33500] [--since 2024-06-01]
  test/.venv/bin/python test/dt_metaplanet_btc.py --fetch-mnav   # mNAV を test/out/strategytracker_all.json に

mNAV（時価総額 ÷ 保有 BTC の時価）は strategytracker.com（メタプラネット公式の analytics.metaplanet.jp の
データ元）の日次。寄りの時点で分かるのは前日までなので、d より前の最新の値を使う。
保有量の更新が「購入日」基準か「開示日」基準かは確かめていない（1 回の購入で保有は数 % しか動かない）。

対象は、市場区分と赤字の除外だけを外した候補（test/dt_btc_days.py の extra_rows と同じ）。
上位 3 に入ったかは問わない（12 件では少なすぎるので、下に窓を開けた日を全部使う）。
BTC は寄りの瞬間に確定している値（d−1 日付の足の終値＝日本時間 d 日 9:00）。
成績は寄り→引け（コスト前）。1 銘柄の日次なので畳む必要はない。
"""

import argparse
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, "test")
from dt_btc_days import BTC, extra_rows  # noqa: E402

ST = "test/out/strategytracker_all.json"
ST_TICKER = {"33500": "3350.T"}


def fetch_mnav():
    import json
    import urllib.request
    base = "https://data.strategytracker.com/"
    latest = json.load(urllib.request.urlopen(base + "latest.json", timeout=30))
    urllib.request.urlretrieve(base + latest["files"]["full"], ST)
    print(f"{ST}: {latest['version']}")


def mnav_series(code):
    import json
    h = json.load(open(ST))["companies"][ST_TICKER[code]]["historicalDataOriginal"]
    return pd.Series(h["nav_premium_basic"], index=pd.to_datetime(h["dates"]), name="mnav").sort_index()


def welch(a, b):
    se = np.sqrt(a.var(ddof=1) / len(a) + b.var(ddof=1) / len(b))
    return (a.mean() - b.mean()) / se


def by_tercile(x, col, label, fmt):
    x = x.copy()
    x["q"] = pd.qcut(x[col], 3, labels=False) + 1
    print(f"\n| {label}の 3 分位 | 範囲 | 日数 | 寄り→引け 平均 | 中央値 | 勝ち |")
    print("|---|---|---|---|---|---|")
    for q, g in x.groupby("q"):
        print(f"| {q} | {fmt(g[col].min())}〜{fmt(g[col].max())} | {len(g)} | {g.y.mean():+.2%} | "
              f"{g.y.median():+.2%} | {(g.y > 0).mean():.0%} |")
    lo, hi = x[x.q == 1].y, x[x.q == 3].y
    print(f"上 − 下 {hi.mean() - lo.mean():+.2%}（Welch t {welch(hi, lo):+.2f}）")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--code", default="33500")
    ap.add_argument("--since", default="2024-06-01")
    ap.add_argument("--fetch-mnav", action="store_true")
    a = ap.parse_args()
    if a.fetch_mnav:
        fetch_mnav()
        return

    x = extra_rows([a.code])
    x = x[x["d"] >= a.since].copy()
    x["y"] = x["y_raw"]

    b = pd.read_csv(BTC, parse_dates=["utc_date"]).set_index("utc_date")["close"]
    b = b[~b.index.duplicated()].asfreq("D").ffill()
    last = x["d"] - pd.Timedelta(days=1)
    x["btc1"] = (b / b.shift(1) - 1).reindex(last).values
    x["btc_px"] = b.reindex(last).values
    m = mnav_series(a.code)
    x["mnav"] = m.reindex(last, method="ffill").values  # 前日の値（週末をまたぐときは直近）
    x = x.dropna(subset=["btc1", "btc_px"])
    # ギャップのうち BTC で説明できない部分（全期間の回帰の残差。探索用）
    beta = np.polyfit(x["btc1"], x["gap"], 1)
    x["gap_resid"] = x["gap"] - np.polyval(beta, x["btc1"])

    print(f"{a.code}: 下に窓を開けた日 {len(x)} 日（{x.d.min().date()}〜{x.d.max().date()}）"
          f"寄り→引け 平均 {x.y.mean():+.2%}・中央値 {x.y.median():+.2%}・勝ち {(x.y > 0).mean():.0%}")
    print(f"ギャップ = {beta[0]:.2f} × BTC 前日 {beta[1]:+.2%}（相関 {x.gap.corr(x.btc1):+.2f}）")

    print("\n| 説明変数 | 寄り→引けとの相関 | t |")
    print("|---|---|---|")
    for col, name in (("btc1", "BTC の前日の騰落"), ("btc_px", "BTC の価格（USD）"), ("o", "株価（始値）"),
                      ("gap", "ギャップ"), ("gap_resid", "ギャップの BTC で説明できない部分"), ("mnav", "mNAV（前日）")):
        r = x.y.corr(x[col])
        print(f"| {name} | {r:+.3f} | {r * np.sqrt((len(x) - 2) / (1 - r * r)):+.2f} |")

    by_tercile(x, "btc1", "BTC の前日の騰落", lambda v: f"{v:+.1%}")
    by_tercile(x, "btc_px", "BTC の価格", lambda v: f"{v:,.0f}")
    by_tercile(x, "o", "株価", lambda v: f"{v:,.0f}")
    by_tercile(x, "mnav", "mNAV", lambda v: f"{v:.2f}")
    print("\n| mNAV | 日数 | 寄り→引け 平均 | 中央値 | 勝ち |")
    print("|---|---|---|---|---|")
    for lo, hi in ((0, 1), (1, 1.5), (1.5, 2.5), (2.5, 99)):
        g = x[(x.mnav >= lo) & (x.mnav < hi)]
        if len(g):
            print(f"| {lo}〜{hi} | {len(g)} | {g.y.mean():+.2%} | {g.y.median():+.2%} | {(g.y > 0).mean():.0%} |")


if __name__ == "__main__":
    main()
