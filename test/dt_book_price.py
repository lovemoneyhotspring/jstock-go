# 気配値は寄値の代理として使えるか。板から値段を取る改修（2026-09-11）の効き目を測る型。
#
# 寄り前・未寄付の銘柄は始値も現在値も空で、値段は最良気配（pQBP / pQAP）にしかない。
# 気配から値段を取れば候補の欠けは埋まるが、**気配値は寄値そのものではない**——板寄せ中の
# 気配は前日終値から徐々に動く仮の値段なので、下落日には寄値より高く出る。ギャップを浅く
# 見せるぶん、深いギャップ（＝利益源）の銘柄が順位で不利になる。
#
# 3 つを並べて測る:
#   現行 … 未寄付を捨てる（改修前）
#   板   … 始値 → 現在値 → 気配の中値（改修後）
#   理想 … 当日の寄値を知っていれば（到達できない上限）
#
# **日数が要る。** 気配のずれの向きはその日の相場の向きで変わるので、1 日では決まらない。
#
#   python test/dt_book_price.py 2026-09-11
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

STATE = os.path.expanduser('~/jstock-go/state/daytrade/history')
SLOTS = ('0900', '0902', '0905')
N = 3

day = sys.argv[1] if len(sys.argv) > 1 else None
if not day:
    print('usage: python test/dt_book_price.py YYYY-MM-DD'); sys.exit(1)

c = con()
for slot in SLOTS:
    df = c.execute(f"""
    WITH p AS (  -- 前夜の母集団。その日の最後の plan を使う
      SELECT * FROM read_parquet('{STATE}/plan/*.parquet', union_by_name=true) pl
      WHERE eligible AND day = DATE '{day}'
        AND run_id = (SELECT run_id FROM read_parquet('{STATE}/plan/*.parquet', union_by_name=true)
                      WHERE day = pl.day AND eligible ORDER BY recorded_at DESC LIMIT 1)),
    bk AS (SELECT symbol, "pDOP" AS o, "pDPP" AS last,
             CASE WHEN "pQBP"<>'' AND "pQAP"<>'' THEN ("pQBP"::DOUBLE + "pQAP"::DOUBLE)/2
                  WHEN "pQBP"<>'' THEN "pQBP"::DOUBLE WHEN "pQAP"<>'' THEN "pQAP"::DOUBLE END AS mid
           FROM read_parquet('{STATE}/book/{day}T*.parquet') WHERE slot = '{slot}'),
    b AS (SELECT Code, O, C FROM bars WHERE Date = DATE '{day}' AND O > 0 AND C > 0)
    SELECT p.symbol, p.name, p.vol20, p.prev_close, b.O AS open, b.C AS close,
           CASE WHEN bk.o<>'' THEN bk.o::DOUBLE WHEN bk.last<>'' THEN bk.last::DOUBLE END AS px_now,
           CASE WHEN bk.o<>'' THEN bk.o::DOUBLE WHEN bk.last<>'' THEN bk.last::DOUBLE ELSE bk.mid END AS px_book,
           bk.mid, (bk.o IS NULL OR bk.o='') AS preopen
    FROM p JOIN b ON b.Code = p.Code JOIN bk ON bk.symbol = p.symbol WHERE p.vol20 > 0""").df()
    if df.empty:
        print(f'{day} {slot}: 記録がありません'); continue
    df['oc_bp'] = (df.close / df.open - 1) * 1e4

    def top(col):
        d = df.dropna(subset=[col]).copy()
        d['g'] = d[col] / d.prev_close - 1
        d = d[d.g < 0]                       # signal.max_gap = 0
        d['k'] = d.g / d.vol20               # signal.rank_by = gap_vol
        return d.nsmallest(N, 'k')

    # 未寄付の銘柄で、気配の中値が実際の寄値からどれだけずれたか
    pre = df[df.preopen & df['mid'].notna()]
    err = (pre['mid'] / pre.open - 1) * 1e4
    print(f"\n=== {day} {slot}  母集団 {len(df)}  未寄付 {len(pre)}")
    if len(pre):
        print(f"  気配の中値 − 寄値: 平均 {err.mean():+.1f} bp  中央値 {err.median():+.1f} bp"
              f"  絶対値の p90 {err.abs().quantile(0.9):.0f} bp")
    for tag, t in (('現行（未寄付を捨てる）', top('px_now')),
                   ('板から取る', top('px_book')),
                   ('理想（寄値を知っていれば）', top('open'))):
        names = " / ".join(f"{r['name'][:8]} {r.oc_bp:+.0f}" for _, r in t.iterrows())
        print(f"  {tag:<22} 上位 {N} の寄→引 {t.oc_bp.mean():+7.1f} bp   {names}")
