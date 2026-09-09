# 相関行列の実用: daytrade が同じ日に建てる 3 銘柄は互いに相関していないか（分散が効いているか）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import json
P = np.load(f'{OUT}/p6_prices.npy'); meta = json.load(open(f'{OUT}/p6_meta.json'))
codes = meta['codes']; ci = {c: i for i, c in enumerate(codes)}; D, G, N = P.shape
R = np.full((D, G, N), np.nan, np.float32)
R[:, 1:30, :] = np.log(P[:, 1:30, :] / P[:, 0:29, :]); R[:, 31:, :] = np.log(P[:, 31:, :] / P[:, 30:-1, :])
R[~np.isfinite(R)] = np.nan
X = R - np.nanmean(R, axis=2, keepdims=True); valid = np.isfinite(X).all(axis=2); X[np.isnan(X)] = 0.0
Z = np.concatenate([X[:, s:e, :][valid[:, s:e]] for s, e in [(1, 30), (31, G)]])
Zn = (Z - Z.mean(0)) / (Z.std(0) + 1e-12); C0 = (Zn.T @ Zn) / len(Zn)
Zr = np.concatenate([R[:, s:e, :][valid[:, s:e]] for s, e in [(1, 30), (31, G)]])
Zrn = (Zr - Zr.mean(0)) / (Zr.std(0) + 1e-12); Craw = (Zrn.T @ Zrn) / len(Zrn)
t = pd.read_csv(f'{OUT}/trades_10y.csv'); t = t[t.date >= '2024-09-02'].copy(); t['code'] = t.code.astype(str)
rng = np.random.default_rng(0)
print(f"母集団 {N} 銘柄の任意の 2 組: 残差 ρ 平均 {C0[~np.eye(N,dtype=bool)].mean():+.3f}  生 ρ 平均 {Craw[~np.eye(N,dtype=bool)].mean():+.3f}")
for side in ['long', 'short']:
    obs_r, obs_w, nd = [], [], 0
    for d, g in t[t.side == side].groupby('date'):
        idx = [ci[c] for c in g.code.unique() if c in ci]
        if len(idx) < 2: continue
        nd += 1
        for a in range(len(idx)):
            for b in range(a + 1, len(idx)):
                obs_r.append(C0[idx[a], idx[b]]); obs_w.append(Craw[idx[a], idx[b]])
    rnd = [C0[i, j] for i, j in rng.integers(0, N, (5000, 2)) if i != j]
    rndw = [Craw[i, j] for i, j in rng.integers(0, N, (5000, 2)) if i != j]
    print(f"  {side:5s}: 同日に 2 銘柄以上を建てた日 {nd:4d}、組 {len(obs_r):5d}  残差 ρ 平均 {np.mean(obs_r):+.3f}（無作為 {np.mean(rnd):+.3f}）  生 ρ 平均 {np.mean(obs_w):+.3f}（無作為 {np.mean(rndw):+.3f}）")
