#!/usr/bin/env python3
"""全面安・全面高の予測（2026-09-jp-market-breadth）のパネルを作る。

目的変数は東証プライム（2022-04 より前は東証一部）の値上がり銘柄比率を 3 通り。
等ウェイトと時価総額加重の両方を作る。
特徴量は日本の当日 09:00 時点で確定しているものだけに限る（リーク防止）。

第 2 版（2026-09-16）で追加:
  - ドル円（FRED DEXJPUS）・米 10 年債（FRED DGS10）
  - SOX（test/out/sox.csv があれば。無ければ黙って飛ばす）
  - 時価総額加重の breadth

出力: test/out/mb_panel.parquet
"""

import json
import os

import duckdb
import numpy as np
import pandas as pd

ROOT = os.path.expanduser("~/jstock-go")
OUT = os.path.join(ROOT, "test", "out")
os.makedirs(OUT, exist_ok=True)

JQ = os.path.join(ROOT, "data", "jquants")


def pq(name: str) -> str:
    return f"read_parquet('{JQ}/{name}/*.parquet', union_by_name=true)"


def fred(series: str, col: str) -> pd.DataFrame:
    """FRED の CSV を読む。休場日は '.' で入っているので落とす。"""
    path = os.path.join(OUT, f"fred_{series}.csv")
    s = pd.read_csv(path)
    s.columns = ["date", col]
    s["date"] = pd.to_datetime(s["date"])
    s[col] = pd.to_numeric(s[col], errors="coerce")
    return s.dropna().sort_values("date").reset_index(drop=True)


def asof_prior(df: pd.DataFrame, right: pd.DataFrame, key: str) -> pd.DataFrame:
    """日本の d 日 09:00 に使えるのは『d より前』の海外データだけ。"""
    r = right.rename(columns={"date": key}).sort_values(key)
    out = pd.merge_asof(df.sort_values("date"), r, left_on="date", right_on=key,
                        direction="backward", allow_exact_matches=False)
    return out.drop(columns=[key])


con = duckdb.connect()

# ---------------------------------------------------------------- breadth
# 前日終値は「プライムだったか」に関係なく銘柄の連続した系列から取る。
# 先に master で絞ると市場区分が変わった銘柄で lag が飛ぶ。
breadth = con.execute(f"""
WITH bars AS (
  SELECT Date, Code,
         TRY_CAST(AdjO AS DOUBLE)   o,
         TRY_CAST(AdjC AS DOUBLE)   c,
         TRY_CAST(Vo  AS DOUBLE)    vo,
         TRY_CAST(MktCap AS DOUBLE) mc
  FROM {pq('equities_bars_daily')}
),
lagged AS (
  SELECT *, lag(c) OVER (PARTITION BY Code ORDER BY Date) pc FROM bars
),
mst AS (
  SELECT Date, Code FROM {pq('equities_master')}
  WHERE MktNm IN ('東証一部', 'プライム')
),
j AS (
  SELECT l.* FROM lagged l JOIN mst m ON m.Date = l.Date AND m.Code = l.Code
  WHERE l.o IS NOT NULL AND l.c IS NOT NULL AND l.pc IS NOT NULL AND l.vo > 0
)
SELECT Date AS date,
       count(*)                                     AS n,
       avg(CASE WHEN o > pc THEN 1.0 ELSE 0.0 END)  AS gap_breadth,
       avg(CASE WHEN c > o  THEN 1.0 ELSE 0.0 END)  AS intraday_breadth,
       avg(CASE WHEN c > pc THEN 1.0 ELSE 0.0 END)  AS day_breadth,
       sum(CASE WHEN c > pc THEN 1 ELSE 0 END)      AS n_up,
       sum(CASE WHEN c < pc THEN 1 ELSE 0 END)      AS n_dn,
       -- 時価総額加重（MktCap が欠けている銘柄は分母からも外す）
       sum(CASE WHEN mc IS NOT NULL AND o > pc THEN mc END)
         / sum(CASE WHEN mc IS NOT NULL THEN mc END) AS cw_gap_breadth,
       sum(CASE WHEN mc IS NOT NULL AND c > o  THEN mc END)
         / sum(CASE WHEN mc IS NOT NULL THEN mc END) AS cw_intraday_breadth,
       sum(CASE WHEN mc IS NOT NULL AND c > pc THEN mc END)
         / sum(CASE WHEN mc IS NOT NULL THEN mc END) AS cw_day_breadth
FROM j GROUP BY 1 ORDER BY 1
""").df()
breadth["date"] = pd.to_datetime(breadth["date"])
print(f"breadth: {len(breadth)} 日  {breadth.date.min().date()} 〜 {breadth.date.max().date()}"
      f"  銘柄数 中央値 {breadth.n.median():.0f}")

df = breadth.copy()

# ---------------------------------------------------------------- TOPIX
topix = con.execute(f"""
SELECT Date AS date, TRY_CAST(O AS DOUBLE) o, TRY_CAST(C AS DOUBLE) c
FROM {pq('indices_bars_daily_topix')} ORDER BY 1
""").df()
topix["date"] = pd.to_datetime(topix["date"])
topix["tpx_ret"] = topix.c.pct_change()
topix["tpx_gap"] = topix.o / topix.c.shift(1) - 1.0          # 当日の寄りギャップ（当日分・参考）
topix["tpx_vol20"] = topix.tpx_ret.rolling(20).std()
topix["tpx_dev25"] = topix.c / topix.c.rolling(25).mean() - 1.0
df = df.merge(topix[["date", "c", "o", "tpx_ret", "tpx_gap", "tpx_vol20", "tpx_dev25"]]
              .rename(columns={"c": "tpx_c", "o": "tpx_o"}), on="date", how="left")

# ------------------------------------------------------- 日経225（オプション）
# UnderPx は日に 1 値、BaseVol も日に 1 値（日経 VI 相当）。IV は限月・行使価格ごとなので使わない。
nk = con.execute(f"""
SELECT Date AS date,
       any_value(TRY_CAST(UnderPx AS DOUBLE))  AS nk_px,
       any_value(TRY_CAST(BaseVol AS DOUBLE))  AS nk_vi
FROM {pq('derivatives_bars_daily_options_225')} GROUP BY 1 ORDER BY 1
""").df()
nk["date"] = pd.to_datetime(nk["date"])
nk["nk_ret"] = nk.nk_px.pct_change()
nk["nk_vi_chg"] = nk.nk_vi.diff()
df = df.merge(nk, on="date", how="left")

# ---------------------------------------------------------------- 空売り比率
sr = con.execute(f"""
SELECT Date AS date,
       sum(TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE))
       / nullif(sum(TRY_CAST(SellExShortVa AS DOUBLE)
                  + TRY_CAST(ShrtWithResVa AS DOUBLE)
                  + TRY_CAST(ShrtNoResVa AS DOUBLE)), 0) AS short_ratio
FROM {pq('markets_short_ratio')} GROUP BY 1 ORDER BY 1
""").df()
sr["date"] = pd.to_datetime(sr["date"])
df = df.merge(sr, on="date", how="left")

# ------------------------------------------------------- 投資部門別（週次・公表ラグあり）
# PubDate が当日より前のものだけを使う。Section はプライム移行で名前が変わる。
inv = con.execute(f"""
SELECT PubDate AS pub_date,
       sum(TRY_CAST(FrgnBal AS DOUBLE)) / nullif(sum(TRY_CAST(FrgnTot AS DOUBLE)), 0) AS frgn_bal_r,
       sum(TRY_CAST(IndBal  AS DOUBLE)) / nullif(sum(TRY_CAST(IndTot  AS DOUBLE)), 0) AS ind_bal_r
FROM {pq('equities_investor_types')}
WHERE Section IN ('TSE1st', 'TSEPrime')
GROUP BY 1 ORDER BY 1
""").df()
inv["pub_date"] = pd.to_datetime(inv["pub_date"])
df = pd.merge_asof(df.sort_values("date"), inv.sort_values("pub_date"),
                   left_on="date", right_on="pub_date",
                   direction="backward", allow_exact_matches=False)

# ---------------------------------------------------------------- 米国（前夜）
us = pd.DataFrame(json.load(open(os.path.join(ROOT, "data", "daytrade", "us.json"))))
us["date"] = pd.to_datetime(us["date"])
us = us.sort_values("date")
us["spx_ret1"] = us.spx.pct_change()
us["spx_ret2"] = us.spx.pct_change(2)
us["vix_chg"] = us.vix.diff()
df = asof_prior(df, us[["date", "spx", "spx_ret1", "spx_ret2", "vix", "vix_chg"]], "us_date")

# ---------------------------------------------------------------- ドル円（FRED DEXJPUS）
# NY 正午の対顧客買い相場。米国 d 日の値は日本の d+1 日 02:00 頃に確定するので、
# 「d より前」の規則で引けばリークしない。
fx = fred("DEXJPUS", "usdjpy")
fx["usdjpy_ret1"] = fx.usdjpy.pct_change()
fx["usdjpy_ret5"] = fx.usdjpy.pct_change(5)
fx["usdjpy_vol20"] = fx.usdjpy_ret1.rolling(20).std()
df = asof_prior(df, fx, "fx_date")

# ---------------------------------------------------------------- 米 10 年債（FRED DGS10）
ust = fred("DGS10", "ust10")
ust["ust10_chg1"] = ust.ust10.diff()
ust["ust10_chg5"] = ust.ust10.diff(5)
df = asof_prior(df, ust, "ust_date")

# ---------------------------------------------------------------- SOX（あれば）
sox_path = os.path.join(OUT, "sox.csv")
if os.path.exists(sox_path):
    sox = pd.read_csv(sox_path)
    sox.columns = ["date", "sox"][:len(sox.columns)]
    sox["date"] = pd.to_datetime(sox["date"])
    sox["sox"] = pd.to_numeric(sox["sox"], errors="coerce")
    sox = sox.dropna().sort_values("date").reset_index(drop=True)
    sox["sox_ret1"] = sox.sox.pct_change()
    sox["sox_ret2"] = sox.sox.pct_change(2)
    df = asof_prior(df, sox, "sox_date")
    print(f"SOX: {len(sox)} 日 {sox.date.min().date()} 〜 {sox.date.max().date()}")
else:
    print("SOX: 取得できていないので使わない")

# ---------------------------------------------------------------- 市場の内部状態
df = df.sort_values("date").reset_index(drop=True)
# 騰落レシオ 25（値上がり数 25 日合計 ÷ 値下がり数 25 日合計）
df["adr25"] = (df.n_up.rolling(25).sum() / df.n_dn.rolling(25).sum().replace(0, np.nan))

# ---------------------------------------------------------------- カレンダー
df["dow"] = df.date.dt.dayofweek
g = df.groupby(df.date.dt.to_period("M"))
df["bd_from_start"] = g.cumcount()                       # 月内の営業日位置（0 始まり）
df["bd_to_end"] = g["date"].transform("size") - 1 - df["bd_from_start"]
df["is_month_start3"] = (df.bd_from_start < 3).astype(int)
df["is_month_end3"] = (df.bd_to_end < 3).astype(int)
# SQ 週: 第 2 金曜を含む週
wk = df.date.dt.isocalendar()
fri2 = (df.dow == 4) & (df.bd_from_start >= 4) & (df.bd_from_start <= 10)
sq_weeks = set(zip(wk.year[fri2], wk.week[fri2]))
df["is_sq_week"] = [1 if (y, w) in sq_weeks else 0 for y, w in zip(wk.year, wk.week)]
df = df.drop(columns=["bd_from_start", "bd_to_end"])

# ---------------------------------------------------------------- ラグ（当日を見ない）
# 日本市場から作った系列は、当日の引けを含むので必ず 1 日ずらす。
JP_LAG = ["tpx_ret", "tpx_vol20", "tpx_dev25", "nk_ret", "nk_vi", "nk_vi_chg",
          "short_ratio", "adr25", "day_breadth", "intraday_breadth", "gap_breadth",
          "cw_day_breadth"]
for col in JP_LAG:
    df[f"p_{col}"] = df[col].shift(1)
df["p5_day_breadth"] = df.day_breadth.shift(5)
df["p_tpx_ret5"] = df.tpx_c.shift(1) / df.tpx_c.shift(6) - 1.0

# ---------------------------------------------------------------- 目的変数
for pre in ["", "cw_"]:
    for kind in ["gap", "intraday", "day"]:
        b = df[f"{pre}{kind}_breadth"]
        df[f"y_{pre}{kind}_down"] = (b <= 0.20).astype(int)
        df[f"y_{pre}{kind}_up"] = (b >= 0.80).astype(int)

path = os.path.join(OUT, "mb_panel.parquet")
df.to_parquet(path, index=False)

print(f"\npanel: {len(df)} 行 × {len(df.columns)} 列 -> {path}")
print(f"期間: {df.date.min().date()} 〜 {df.date.max().date()}")
miss = df.isna().mean().sort_values(ascending=False)
print("\n欠損率 上位:")
print(miss[miss > 0].head(12).round(4).to_string())
