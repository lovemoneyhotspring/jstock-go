# 寄り付き時刻とギャップの関係。9 時何分に、どのギャップ帯を選べば効率的か。
#
# 段階 1: 分足から銘柄ごとの「最初に約定した分」を作って保存する（重いので 1 回だけ）。
# 分足は約定があった分にしか行が無いので、最初の行の Time がその銘柄の寄り付き時刻
# （Time は足の開始時刻。09:00 の足に板寄せの出来高が入る）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

CACHE = f'{OUT}/open_times.parquet'
c = con()
if not os.path.exists(CACHE):
    print('分足から寄り付き時刻を作る（数分かかる）…')
    c.execute(f"""
    COPY (
      WITH m AS (
        SELECT Date, Code, Time, TRY_CAST(O AS DOUBLE) O, TRY_CAST(Vo AS DOUBLE) Vo
        FROM read_parquet('{ROOT}/equities_bars_minute/*.parquet', union_by_name=true)
        WHERE Time <= '09:30' AND TRY_CAST(Vo AS DOUBLE) > 0)
      SELECT Date, Code,
             min(Time) AS open_time,
             arg_min(O, Time) AS open_px
      FROM m GROUP BY Date, Code
    ) TO '{CACHE}' (FORMAT PARQUET)""")
print('寄り付き時刻:', CACHE)
print(c.execute(f"SELECT count(*) n, min(Date) f, max(Date) t FROM read_parquet('{CACHE}')").df().to_string())

# 段階 2: 母集団（プライム・売買代金 20 日中央値 1 億円以上・時価総額の下位 1/3 を外す）に
# 絞り、ギャップ帯 × 寄り付き時刻の表を作る。決算の除外は入れていない（寄り付き時刻の
# 分布を見るのが目的で、選定の再現ではない）。
UNIV = f'{OUT}/open_gap.parquet'
if not os.path.exists(UNIV):
    c.execute(f"""
    COPY (
      WITH r AS (
        SELECT Date, Code, AdjO, AdjC, Va, MktCap,
               LAG(AdjC) OVER (PARTITION BY Code ORDER BY Date) AS pc
        FROM bars WHERE AdjO > 0 AND AdjC > 0),
      d AS (
        SELECT *, median(Va) OVER w AS turn,
               stddev_samp(AdjC / NULLIF(pc, 0) - 1) OVER w AS vol20
        FROM r WINDOW w AS (PARTITION BY Code ORDER BY Date ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING)),
      m AS (SELECT Code, S33, Mkt FROM master WHERE Date = (SELECT max(Date) FROM master)),
      t AS (SELECT Date, Code, MktCap,
                   NTILE(3) OVER (PARTITION BY Date ORDER BY MktCap) AS cap_tercile FROM d WHERE MktCap > 0)
      SELECT d.Date, d.Code, o.open_time, o.open_px,
             d.AdjO / d.pc - 1 AS gap, d.AdjC / d.AdjO - 1 AS oc, d.vol20, d.turn, t.cap_tercile
      FROM d
      JOIN read_parquet('{CACHE}') o ON o.Date = d.Date AND o.Code = d.Code
      JOIN m ON m.Code = d.Code
      JOIN t ON t.Date = d.Date AND t.Code = d.Code
      WHERE d.pc > 0 AND d.turn >= 1e8 AND d.vol20 > 0 AND t.cap_tercile > 1
        AND m.Mkt = '0111'   -- プライム
    ) TO '{UNIV}' (FORMAT PARQUET)""")
df = c.execute(f"SELECT * FROM read_parquet('{UNIV}')").df()
print(f"\n母集団 {len(df):,} 行  {df.Date.min()}〜{df.Date.max()}  1 日あたり {len(df)/df.Date.nunique():.0f} 銘柄")

# 段階 3: 決定時刻ごとの値段（その時刻までの最後の約定値）。9:0T に成行を出すなら、
# 建値はその時点の値段になる——寄値では買えない（既に寄った銘柄を後から買うぶん、
# 最初の 1 分の反発を払う。2026-09-jp-gap-minute の発見 1）。
TIMES = ['09:01', '09:03', '09:05', '09:07', '09:10', '09:15']
PX = f'{OUT}/open_px_at.parquet'
if not os.path.exists(PX):
    cols = ",\n".join(
        f"       max_by(TRY_CAST(C AS DOUBLE), Time) FILTER (WHERE Time <= '{t}') AS px_{t.replace(':','')}"
        for t in TIMES)
    c.execute(f"""
    COPY (
      SELECT Date, Code,
{cols}
      FROM read_parquet('{ROOT}/equities_bars_minute/*.parquet', union_by_name=true)
      WHERE Time <= '09:15' AND TRY_CAST(Vo AS DOUBLE) > 0
      GROUP BY Date, Code
    ) TO '{PX}' (FORMAT PARQUET)""")
print('決定時刻ごとの値段:', PX)
