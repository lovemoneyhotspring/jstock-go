# 検証 4 本（E/C/A/B）で共有する読み込みと指標。親ノート: vault/20-research/2026-09-beyond-index-plan.md
import duckdb, numpy as np, pandas as pd, os

ROOT = os.path.expanduser('~/jstock-go/data/jquants')
WB = os.path.expanduser('~/webull/wbjp/data/bars')
OUT = os.path.expanduser('~/jstock-go/test/out')
os.makedirs(OUT, exist_ok=True)

IS_END = pd.Timestamp('2022-12-31')   # IS = 〜2022-12, OOS = 2023-01〜
LONG_IS_END = pd.Timestamp('2015-12-31')  # 30 年ある指数用

def con():
    c = duckdb.connect()
    c.execute(f"CREATE VIEW bars AS SELECT Date, Code, TRY_CAST(O AS DOUBLE) O, TRY_CAST(H AS DOUBLE) H, TRY_CAST(L AS DOUBLE) L, TRY_CAST(C AS DOUBLE) C, TRY_CAST(Vo AS DOUBLE) Vo, TRY_CAST(Va AS DOUBLE) Va, TRY_CAST(MktCap AS DOUBLE) MktCap, TRY_CAST(AdjO AS DOUBLE) AdjO, TRY_CAST(AdjC AS DOUBLE) AdjC, TRY_CAST(AdjFactor AS DOUBLE) AdjFactor FROM read_parquet('{ROOT}/equities_bars_daily/*.parquet', union_by_name=true)")
    c.execute(f"CREATE VIEW topix AS SELECT Date, TRY_CAST(O AS DOUBLE) O, TRY_CAST(C AS DOUBLE) C FROM read_parquet('{ROOT}/indices_bars_daily_topix/*.parquet', union_by_name=true)")
    c.execute(f"CREATE VIEW master AS SELECT * FROM read_parquet('{ROOT}/equities_master/*.parquet', union_by_name=true)")
    c.execute(f"CREATE VIEW fins AS SELECT * FROM read_parquet('{ROOT}/fins_summary/*.parquet', union_by_name=true)")
    c.execute(f"CREATE VIEW cal AS SELECT Date, HolDiv FROM read_parquet('{ROOT}/markets_calendar/*.parquet', union_by_name=true)")
    return c

def jp_etf(c, code):
    df = c.execute(f"SELECT Date date, AdjO AS o, AdjC AS c FROM bars WHERE Code='{code}' AND AdjO>0 AND AdjC>0 ORDER BY Date").df()
    return df.rename(columns={'o':'open','c':'close'}).set_index(pd.to_datetime(df.date)).drop(columns='date')

def topix(c):
    df = c.execute("SELECT Date date, O AS o, C AS c FROM topix WHERE O>0 ORDER BY Date").df()
    return df.rename(columns={'o':'open','c':'close'}).set_index(pd.to_datetime(df.date)).drop(columns='date')

def wb(symbol):
    df = pd.read_parquet(f'{WB}/{symbol}.parquet')[['date','open','close']]
    df['date'] = pd.to_datetime(df.date).dt.tz_localize(None)
    return df.set_index('date').sort_index()

def irx():
    df = pd.read_parquet(f'{WB}/^IRX.parquet')[['date','close']]
    df['date'] = pd.to_datetime(df.date).dt.tz_localize(None)
    return df.set_index('date').close.sort_index() / 100.0

def stats(r, n=245):
    """日次リターン系列 → CAGR / 最大DD / Sharpe"""
    r = r.dropna()
    if len(r) < 20: return dict(cagr=np.nan, dd=np.nan, sharpe=np.nan, years=0)
    eq = (1 + r).cumprod()
    yrs = len(r) / n
    cagr = eq.iloc[-1] ** (1 / yrs) - 1
    dd = (eq / eq.cummax() - 1).min()
    sh = r.mean() / r.std() * np.sqrt(n) if r.std() > 0 else np.nan
    return dict(cagr=cagr * 100, dd=-dd * 100, sharpe=sh, years=yrs)

def split(r, is_end=IS_END):
    return r[r.index <= is_end], r[r.index > is_end]

def fmt(d):
    return f"CAGR {d['cagr']:6.2f}%  DD {d['dd']:5.1f}%  Sharpe {d['sharpe']:5.2f}  ({d['years']:.1f}y)"
