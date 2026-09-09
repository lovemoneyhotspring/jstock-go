# 銘柄間の先行・追従（lead-lag）。流動性上位の 5 分足の格子を作り、A の t での動きが B の t+5/10/15 分を予測するかを測る。
# 罠: 約定が飛ぶ銘柄は「遅れて動いたように見える」だけ（非同期取引）。流動性で絞り、前半で選んだ組が後半でも残るかで判定する。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import glob, time as _t

c = con()
codes = c.execute("""
SELECT Code FROM (SELECT Code, median(TRY_CAST(Va AS DOUBLE)) va FROM read_parquet('%s/equities_bars_daily/*.parquet', union_by_name=true)
WHERE Date >= '2024-09-01' GROUP BY Code) WHERE va >= 1e9 ORDER BY Code""" % ROOT).df().Code.tolist()
print('codes', len(codes), flush=True)
cidx = {cd: i for i, cd in enumerate(codes)}
# 5 分の格子（前場 09:05〜11:30 の 30 点、後場 12:35〜15:30 の 36 点）
grid = [f"{9 + (5 * k + 5) // 60:02d}:{(5 * k + 5) % 60:02d}" for k in range(30)]           # 09:05..11:30
grid += [f"{12 + (35 + 5 * k) // 60:02d}:{(35 + 5 * k) % 60:02d}" for k in range(36)]        # 12:35..15:30
gidx = {g: i for i, g in enumerate(grid)}
G = len(grid)
files = sorted(glob.glob(f'{ROOT}/equities_bars_minute/*.parquet'))
P = np.full((len(files), G, len(codes)), np.nan, np.float32)
t0 = _t.time()
for di, f in enumerate(files):
    d = c.execute(f"SELECT Code, Time, TRY_CAST(C AS DOUBLE) C FROM read_parquet('{f}') WHERE TRY_CAST(Vo AS DOUBLE) > 0 AND Time <= '15:30'").df()
    d = d[d.Code.isin(cidx)]
    if not len(d): continue
    mins = d.Time.str.slice(0, 2).astype(int) * 60 + d.Time.str.slice(3, 5).astype(int)
    # その分を含む格子（前場は 11:30 まで、後場は 12:35 以降）
    g = np.where(mins <= 690, np.ceil((mins - 540) / 5) * 5 + 540, np.where(mins >= 750, np.ceil((mins - 755) / 5) * 5 + 755, -1))
    d = d.assign(g=g)[lambda x: x.g >= 545]
    d['gi'] = ((d.g - 545) / 5).astype(int)
    d.loc[d.g > 690, 'gi'] = 30 + ((d.g[d.g > 690] - 755) / 5).astype(int)
    d = d[(d.gi >= 0) & (d.gi < G)]
    last = d.groupby(['gi', 'Code']).C.last()
    ii = last.index.get_level_values(0).values
    jj = np.array([cidx[x] for x in last.index.get_level_values(1)])
    P[di, ii, jj] = last.values
    if di % 100 == 0: print(di, f"{_t.time()-t0:.0f}s", flush=True)
# 前方補完（同じ日・同じ場の中だけ）。補完率を記録
filled = np.isnan(P)
for s, e in [(0, 30), (30, G)]:
    for g in range(s + 1, e):
        m = np.isnan(P[:, g, :]); P[:, g, :][m] = P[:, g - 1, :][m]
cover = 1 - filled.mean(axis=(0, 1))
keep = cover >= 0.95
print(f"補完前の充足率 中央値 {np.median(cover):.3f}、95% 以上の銘柄 {keep.sum()} / {len(codes)}", flush=True)
P = P[:, :, keep]; codes = [cd for cd, k in zip(codes, keep) if k]
np.save(f'{OUT}/p6_prices.npy', P)
import json; json.dump(dict(codes=codes, files=[f.rsplit('/', 1)[1][:10] for f in files], grid=grid), open(f'{OUT}/p6_meta.json', 'w'))
print('saved', P.shape, flush=True)
