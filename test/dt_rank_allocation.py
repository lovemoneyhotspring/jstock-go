"""順位への資金の配り方（事前登録: vault 20-research/2026-09-jp-daytrade-rank-allocation.md）。

設定（test/out/ は git の管理外なので、ここに書いておく）:
  test/out/cfg_wide_lb/daytrade.toml  extends = "../../../config/daytrade_margin"
  test/out/cfg_wide_gv/daytrade.toml  extends = "../cfg_gv"（dt_short_account_dd.py の冒頭）
  どちらも [capital] max_capital = 10000000000 / max_positions = 30 / name_divisor = 1 / max_order = 0
            [signal] max_per_sector = 0
  → 上位 30 位まで、1 銘柄は売買代金 × 0.2% いっぱいに建てる。amount ÷ scale がその銘柄の上限額。
    業種の上限（本番は 1 業種 1 銘柄）は、1 単元が載るかで結果が変わるのでここで掛け直す
    （広い設定のまま掛けると、本番では載らない値がさ株が業種の枠を取ってしまう）

  bash test/heavy.sh ./bin/daytrade backtest --config-dir test/out/cfg_wide_lb --trades-csv test/out/wide_lb.csv
  bash test/heavy.sh ./bin/daytrade backtest --config-dir test/out/cfg_wide_gv --trades-csv test/out/wide_gv.csv
  test/.venv/bin/python test/dt_rank_allocation.py [名前=wide.csv:本物.csv ...]

本物の R7（test/out/mon_trades.csv・gv_trades.csv）と日次で突き合わせてから比べる。
"""
import sys

import numpy as np
import pandas as pd

from common import con

IS_END = pd.Timestamp("2021-12-31")
# 1 日の総額: 平常日はロング 500 万 ＋ 停止中のショートの枠 200 万（spill_to_long）＝ 700 万。
# ショック日はショートが倍率 0 で回らず 500 万。倍率（scale）は backtest と同じく配分を決めた後で金額に掛ける
TOTAL_NORMAL, TOTAL_SHOCK = 7_000_000, 5_000_000
LOT = 100


def tilt(ws):
    s = sum(ws)
    return [w / s for w in ws]


FORMS = {
    "R7": [1 / 7] * 10,
    "R6": [1 / 6] * 6,
    "F6": [1 / 4] * 6,
    "T6": tilt([6, 5, 4, 3, 2, 1]),
    "M6": tilt([1.0, 0.9, 0.8, 0.7, 0.6, 0.5]),
}
SETS = {"LBZ2（インサンプル）": ("test/out/wide_lb.csv", "test/out/mon_trades.csv"),
        "gap_vol": ("test/out/wide_gv.csv", "test/out/gv_trades.csv")}
if len(sys.argv) > 1:
    SETS = {a.split("=", 1)[0]: tuple(a.split("=", 1)[1].split(":")) for a in sys.argv[1:]}


def simulate(w, weights):
    """1 日ごとに順位順に埋める。上限 = min(売買代金 × 0.2%, 総額 × 重み, 残り)。1 単元が載らなければ次点。"""
    out = {}
    for day, g in w.groupby("date", sort=True):
        scale = g.scale.iloc[0]
        total = TOTAL_NORMAL if scale == 1 else TOTAL_SHOCK
        left, k, pnl = total, 0, 0.0
        used = set()
        for r in g.itertuples():
            if k >= len(weights):
                break
            lim = min(r.cap, total * weights[k])
            if np.floor(lim / (r.entry * LOT)) < 1:
                continue  # 1 単元が上限に載らない: 次点を繰り上げる（順位の枠は使わない）
            if isinstance(r.sector, str) and r.sector:
                if r.sector in used:
                    continue  # 同じ業種は 1 銘柄まで（signal.max_per_sector = 1）
                used.add(r.sector)
            sh = np.floor(min(lim, left) / (r.entry * LOT)) * LOT
            k += 1
            if sh <= 0:
                continue
            amt = sh * r.entry
            left -= amt
            pnl += amt * r.bp / 1e4
        out[day] = pnl * scale
    return pd.Series(out)


def sectors(w):
    """その日の 33 業種（equities/master の S33。backtest と同じ）。"""
    c = con()
    c.register("k", w[["date", "code"]].drop_duplicates().astype({"code": str}))
    s = c.execute("""SELECT k.date, k.code, CAST(m.S33 AS VARCHAR) sector FROM k
                     JOIN master m ON CAST(m.Date AS DATE) = CAST(k.date AS DATE) AND CAST(m.Code AS VARCHAR) = k.code""").df()
    s["date"] = pd.to_datetime(s.date)
    s["code"] = s.code.astype(w.code.dtype)
    return s


def stats(p):
    eq = p.cumsum()
    return p.sum(), p.mean() / p.std() * np.sqrt(245), (eq - eq.cummax()).min()


def tstat(d):
    return d.mean() / d.std() * np.sqrt(len(d))


def main():
    pd.set_option("display.width", 200)
    for name, (wide, real) in SETS.items():
        w = pd.read_csv(wide, parse_dates=["date"])
        w = w[w.side == "long"].sort_values(["date", "rank"])
        w["cap"] = w.amount / w.scale
        w = w.merge(sectors(w), on=["date", "code"], how="left")
        w["bp"] = w.pnl / w.amount * 1e4
        res = {f: simulate(w, ws) for f, ws in FORMS.items()}
        idx = res["R7"].index

        rl = pd.read_csv(real, parse_dates=["date"])
        rl = rl[rl.side == "long"].groupby("date").pnl.sum().reindex(idx, fill_value=0.0)
        corr = res["R7"].corr(rl)
        gap = res["R7"].sum() / rl.sum() - 1
        ok = corr >= 0.95 and abs(gap) <= 0.05
        print(f"== {name}  突き合わせ: 近似 R7 と本物の日次相関 {corr:.3f}・損益の差 {gap:+.1%} → {'○' if ok else '× 判定しない'}")

        rows = []
        for f, p in res.items():
            d = p - res["R7"]
            row = {"形": f}
            for per, m in (("IS", idx <= IS_END), ("OOS", idx > IS_END)):
                s = stats(p[m])
                row.update({f"{per}損益": s[0], f"{per}Sharpe": s[1], f"{per}DD": s[2], f"{per}差t": tstat(d[m]) if f != "R7" else np.nan})
            rows.append(row)
        r = pd.DataFrame(rows).set_index("形")
        base = r.loc["R7"]
        r["採用"] = [(f != "R7") and x.OOS損益 > base.OOS損益 and x.OOSSharpe > base.OOSSharpe and x.OOS差t >= 2
                     and x.IS差t > 0 and x.OOSDD >= base.OOSDD * 1.10 for f, x in r.iterrows()]
        print(r.round({c: 0 for c in r.columns if "損益" in c or "DD" in c} | {c: 2 for c in r.columns if "Sharpe" in c or "差t" in c}).to_string())
        print()


if __name__ == "__main__":
    main()
