"""daytrade ショートの候補表（寄付のギャップアップを売って 15:20 に買い戻す）を backtest のパネルキャッシュから作り直す。

根拠: vault 20-research/2026-09-jp-daytrade-retry-unopened.md「母集団と期間」・2026-09-jp-daytrade-preopen-order.md
（元の /tmp/dt_retry_unopened.py build が作った /tmp/ru_short_cand.parquet と、/tmp/pre_ticks.parquet は消えたので、
ノートの条件から書き直したもの。2026-09-26）

  test/.venv/bin/python test/dt_short_candidates.py [--panel パネル] [--out 出力] [--min-gap 0.03]

規則は本番のショート脚（config/daytrade_margin の [margin]・universe.ShortFilter）と同じ:
  プライム・貸借・売買代金の 20 日中央値 1 億以上・決算（前日引け後／当日）を除く・売り禁（日証金の申込停止）を除く。
  日々公表などの規制（alert）は外さない（exclude_margin_alert = false）。時価総額の 3 分位も外さない。
  始値のギャップ [min_gap, +100%)。既定の min_gap は +3%: 選ぶ帯は +5% 以上だが、気配の誤差で +3〜+5% の銘柄が
  +5% 以上に見えることがある（元の検証の誤差の表にも +3〜+5% の帯がある）。+5% で切った表は取引日が 204 日で、
  2026-09-19 のノートの 249 日に届かない（+3% なら 242 日。再現の数字は +3% の方が近い。2026-09-26）。
  **ストップ高寄りも残す**（気配の模擬で、見える値段がストップ高でなければ選ばれうるため）。
  1 単元が予算内かどうかは選ぶ側（見える値段）で見る。TOB などの除外（corp_events）は過去分が無いので入れない。

列（候補表の 1 行 = 1 日 × 1 銘柄）:
  gap・o・c・prev_close・next_open   日足（パネル）
  px1520   手仕舞いの値段。15:20 の分足の始値、その足が無ければ 15:20 より前の最後の足の終値
           （2024-11-05 より前は 15:20 の足が無いので、引けまでの最後の終値＝ほぼ引け）
  open_t   ティックの最初の約定の時刻（'HH:MM:SS.ffffff'、9:01 より前に約定が無ければ空）
  open_px  その約定値。t06 は 9:00:06 以後の最初の約定値（9:01 より前）。「今の形」の建値は o × t06 / open_px
  limit_up 前日終値を基準値段としたストップ高の値段（pkg/wbcore/marketrules の値幅表）
"""

import argparse
import glob
import os

import duckdb
import numpy as np
import pandas as pd

from dt_candidates import LIMIT_BOUNDS, LIMIT_WIDTHS

MIN_TURNOVER = 1e8
START = "2024-09-02"          # 分足・ティックのある最初の日
OUT = "test/out/dt_short_candidates.parquet"
MINUTE = "data/jquants/equities_bars_minute"
TRADES = "data/jquants/equities_trades"


def limit_up(prev_close):
    width = LIMIT_WIDTHS[np.searchsorted(LIMIT_BOUNDS, prev_close, side="right")]
    return prev_close + width


def per_day(pairs, root, sql):
    """日ごとのファイルに sql を当てて縦に積む。pairs（d・code）にある銘柄だけ読む。"""
    out = []
    con = duckdb.connect()
    for d, g in pairs.groupby("d"):
        f = f"{root}/{d:%Y-%m-%d}.parquet"
        if not os.path.exists(f):
            continue
        con.register("want", g[["code"]])
        out.append(con.sql(sql.format(f=f)).df().assign(d=d))
        con.unregister("want")
    return pd.concat(out, ignore_index=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--panel", default="")
    ap.add_argument("--out", default=OUT)
    ap.add_argument("--min-gap", type=float, default=0.03,
                    help="始値のギャップの下限。0.05 にすると本番の帯と同じだが、気配で +5%% 以上に見える浅い銘柄が落ちる")
    a = ap.parse_args()
    panel = a.panel or max(glob.glob("data/jquants/_panel_cache/panel-*.parquet"), key=os.path.getmtime)
    df = pd.read_parquet(panel, columns=["d", "code", "o", "c", "prev_close", "next_open", "next_open_d", "vol20",
                                         "segment", "shortable", "turnover_med", "earn_prev", "disc_today", "alert",
                                         "jsf_stop", "sector"])
    df["d"] = pd.to_datetime(df["d"])
    c = df[(df["d"] >= START) & (df["segment"] == "prime") & df["shortable"].fillna(False)
           & (df["turnover_med"] >= MIN_TURNOVER) & (df["prev_close"] > 0) & (df["o"] > 0)
           & ~df["earn_prev"].fillna(False) & ~df["disc_today"].fillna(False) & ~df["jsf_stop"].fillna(False)].copy()
    c["gap"] = c["o"] / c["prev_close"] - 1
    c = c[(c["gap"] >= a.min_gap) & (c["gap"] < 1.0)].sort_values(["d", "code"], kind="mergesort").reset_index(drop=True)
    c["limit_up"] = limit_up(c["prev_close"].values)

    mins = per_day(c, MINUTE, """
        SELECT CAST(Code AS VARCHAR) code,
               coalesce(arg_min(O, Time) FILTER (WHERE Time >= '15:20' AND Time < '15:21'),
                        arg_max(C, Time) FILTER (WHERE Time < '15:20')) px1520
        FROM read_parquet('{f}') WHERE CAST(Code AS VARCHAR) IN (SELECT code FROM want) GROUP BY 1""")
    ticks = per_day(c, TRADES, """
        SELECT CAST(Code AS VARCHAR) code, min(Time) open_t, arg_min(Price, Time) open_px,
               arg_min(Price, Time) FILTER (WHERE Time >= '09:00:06') t06
        FROM read_parquet('{f}') WHERE Time < '09:01' AND CAST(Code AS VARCHAR) IN (SELECT code FROM want) GROUP BY 1""")
    c = c.merge(mins, on=["d", "code"], how="left").merge(ticks, on=["d", "code"], how="left")
    c.to_parquet(a.out, index=False)
    days = c.groupby("d").size()
    print(f"{panel}\n→ {a.out}: {len(c):,} 行 / {len(days):,} 日（{c['d'].min():%Y-%m-%d}〜{c['d'].max():%Y-%m-%d}）、"
          f"1 日あたり中央値 {int(days.median())} 本、ストップ高寄り {(c['o'] >= c['limit_up']).mean() * 100:.1f}%、"
          f"15:20 の値なし {c['px1520'].isna().sum()} 行、9:01 までのティックなし {c['open_t'].isna().sum()} 行、"
          f"9:00:03 より前に寄った {(c['open_t'] < '09:00:03').mean() * 100:.1f}%")


if __name__ == "__main__":
    main()
