"""引けストップ高で張り付いたショートの返済買いが、引けの板寄せ（ストップ配分）で何割約定しえたか（E、carry_penalty の実測の目安）。

根拠と事前登録: vault 20-research/2026-09-jp-daytrade-short-reinforce.md（2026-09-26）。規則（取り消さずに引けに残す）は
[[2026-09-jp-daytrade-stuck-exit-at-stop]]、1.0 が最悪の仮定なのは [[2026-09-jp-short-dd]]。

  test/.venv/bin/python test/dt_short_candidates.py
  test/.venv/bin/python test/dt_short_carry_fill.py            # 数十秒。carry_penalty の目安を出す
  （続けて Go の backtest。手順は vault のノート）

(a) 2 年（分足）: 引けがストップ高の銘柄日のうち、引けの板寄せ（2024-11-05 から 15:30、それより前は 15:00 の分足）に
    ストップ高の値段で出来高があった割合。あわせて 15:20 の値段がストップ高未満（15:20 の成行はその場で約定する）の割合
(b) 板のある日（slot 1519、2026-09-11〜）: 15:19 の最良買気配がストップ高なら Q = pBV、15:20〜15:24 のストップ高での出来高 V_c、
    引けの板寄せの出来高 V_a として f = min(1, V_a ÷ max(Q − V_c, V_a))（出来高 0 なら f = 0）。15:19 より後の買いを数えないので上振れ
carry_penalty の目安 = 1 − (a) × E[f | 出来高あり]（引けで手仕舞う形）。15:20 で手仕舞う形は、15:20 に張り付いていない事象を
約定済み（carry 0）として数えた値も出す
"""

import glob
import os

import duckdb
import numpy as np
import pandas as pd

from dt_candidates import LIMIT_BOUNDS, LIMIT_WIDTHS  # noqa: F401 （値幅表は dt_short_candidates.limit_up 経由）
from dt_short_candidates import limit_up

CAND = "test/out/dt_short_candidates.parquet"
MINUTE = "data/jquants/equities_bars_minute"
BOOK = "state/daytrade/history/book/*.parquet"
CLOSE_1530 = pd.Timestamp("2024-11-05")
UNTIL = "2026-09-16"
OUT = "test/out/dt_short_carry_fill.csv"


def close_bars(ev):
    """事象（d・code・lu）ごとに、引けの板寄せの出来高 V_a（ストップ高の値段のとき）と 15:20〜15:24 のストップ高での出来高 V_c、15:20 の値段。"""
    out = []
    con = duckdb.connect()
    for d, g in ev.groupby("d"):
        f = f"{MINUTE}/{d:%Y-%m-%d}.parquet"
        if not os.path.exists(f):
            continue
        ct = "15:30" if d >= CLOSE_1530 else "15:00"
        con.register("want", g[["code", "lu"]])
        out.append(con.sql(f"""
            SELECT w.code,
                   coalesce(sum(b.Vo) FILTER (WHERE b.Time = '{ct}' AND b.C >= w.lu - 1e-6), 0) va,
                   coalesce(sum(b.Vo) FILTER (WHERE b.Time >= '15:20' AND b.Time < '15:25' AND b.L >= w.lu - 1e-6), 0) vc,
                   coalesce(arg_min(b.O, b.Time) FILTER (WHERE b.Time >= '15:20' AND b.Time < '15:21'),
                            arg_max(b.C, b.Time) FILTER (WHERE b.Time < '15:20')) px1520
            FROM want w LEFT JOIN read_parquet('{f}') b ON CAST(b.Code AS VARCHAR) = w.code
            GROUP BY 1""").df().assign(d=d))
        con.unregister("want")
    return pd.concat(out, ignore_index=True)


def two_year(ev, label):
    ev = ev.merge(close_bars(ev), on=["d", "code"], how="left")
    ev["has_va"] = ev["va"] > 0
    post = ev["d"] >= CLOSE_1530
    ev["pinned1520"] = np.where(post, ev["px1520"] >= ev["lu"] - 1e-6, True)
    a = ev["has_va"].mean()
    p20 = ev.loc[post, "pinned1520"].mean()
    print(f"(a) {label}: 引けストップ高 {len(ev):,} 件（{ev['d'].nunique()} 日）、引けの板寄せに出来高あり {a * 100:.1f}%、"
          f"15:20 に張り付いていた {p20 * 100:.1f}%（15:30 引けの期間 {post.sum()} 件）、"
          f"張り付いていた事象の出来高あり {ev.loc[post & ev['pinned1520'], 'has_va'].mean() * 100:.1f}%")
    return ev, a, p20


def calibrate():
    """板のある日の全銘柄（売買代金 1 億以上）の引けストップ高で f を測る。"""
    b = duckdb.sql(f"""
        SELECT CAST(day AS DATE) d, symbol || '0' code, TRY_CAST(pQBP AS DOUBLE) bid, TRY_CAST(pBV AS DOUBLE) q,
               TRY_CAST(pQAP AS DOUBLE) ask, TRY_CAST(pAV AS DOUBLE) qa
        FROM read_parquet('{BOOK}', union_by_name=true) WHERE slot = '1519'""").df()
    b["d"] = pd.to_datetime(b["d"])
    b = b.drop_duplicates(["d", "code"])
    panel = max(glob.glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    p = duckdb.sql(f"""SELECT CAST(d AS DATE) d, code, c, prev_close, segment, turnover_med FROM read_parquet('{panel}')
                       WHERE CAST(d AS DATE) >= DATE '2026-09-11' AND turnover_med >= 1e8 AND prev_close > 0""").df()
    p["d"] = pd.to_datetime(p["d"])
    p["lu"] = limit_up(p["prev_close"].values)
    ev = p[p["c"] >= p["lu"] - 1e-6].merge(b, on=["d", "code"], how="left")
    ev = ev.merge(close_bars(ev[["d", "code", "lu"]]), on=["d", "code"], how="left")
    ev["bid_at_lu"] = ev["bid"] >= ev["lu"] - 1e-6
    ev["f"] = np.where(ev["va"] > 0, np.minimum(1.0, ev["va"] / np.maximum(ev["q"].fillna(0) - ev["vc"], ev["va"])), 0.0)
    ev["sell_share"] = ev["qa"] / ev["q"]
    at = ev[ev["bid_at_lu"]]
    print(f"(b) 板のある日 {ev['d'].nunique()} 日: 引けストップ高 {len(ev)} 件（プライム {int((ev['segment'] == 'prime').sum())}）、"
          f"15:19 に買気配がストップ高 {len(at)} 件、うち引けの板寄せに出来高あり {(at['va'] > 0).mean() * 100:.1f}%")
    for lab, x in (("全区分", at), ("プライム", at[at["segment"] == "prime"])):
        xv = x[x["va"] > 0]
        print(f"    {lab}: f の平均 {x['f'].mean():.3f}（出来高ありで {xv['f'].mean():.3f}・中央値 {xv['f'].median():.3f}・"
              f"n {len(xv)}）、15:19 の売気配 ÷ 買気配の中央値 {x['sell_share'].median():.3f}、"
              f"V_a ÷ 15:19 の売気配の中央値 {(xv['va'] / xv['qa']).median():.2f}、V_c の合計 {x['vc'].sum():,.0f} 株")
    return ev, at


def main():
    c = pd.read_parquet(CAND)
    c = c[c["d"] <= UNTIL]
    ev_c = c[c["c"] >= c["limit_up"] - 1e-6][["d", "code", "limit_up", "gap"]].rename(columns={"limit_up": "lu"})
    ev_c5 = ev_c[ev_c["gap"] >= 0.05]
    _, a3, p3 = two_year(ev_c, "候補表（+3% 以上）")
    e5, a5, p5 = two_year(ev_c5, "候補表（+5% 以上）")
    cal, at = calibrate()
    fv = at.loc[at["va"] > 0, "f"].mean()
    fv_prime = at.loc[(at["va"] > 0) & (at["segment"] == "prime"), "f"].mean()
    rows = []
    for lab, a, p20 in (("+3%", a3, p3), ("+5%", a5, p5)):
        for fl, f in (("全区分", fv), ("プライム", fv_prime)):
            close_form = 1 - a * f
            rows.append(dict(cand=lab, f_from=fl, has_va=a, f_given_va=f, carry_close=close_form,
                             pinned1520=p20, carry_1520=p20 * close_form))
    r = pd.DataFrame(rows)
    r.to_csv(OUT, index=False)
    print("\ncarry_penalty の目安（引けで手仕舞う形 = 1 − (a) × E[f | 出来高あり]、15:20 の形 = 15:20 に張り付いていた割合 × それ）")
    print(r.round(3).to_string(index=False))
    cal.to_csv("test/out/dt_short_carry_fill_calib.csv", index=False)


if __name__ == "__main__":
    main()
