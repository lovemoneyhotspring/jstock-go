# 銘柄間の先行・追従の判定。5 分足の残差リターンで lag 0〜3（0/5/10/15 分）の相関を測り、
# (1) 見かけの大きさ、(2) 前半で選んだ組が後半でも残るか、(3) bp に直すといくらか、を出す。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import json
P = np.load(f'{OUT}/p6_prices.npy'); meta = json.load(open(f'{OUT}/p6_meta.json'))
codes = meta['codes']; days = meta['files']; grid = meta['grid']
D, G, N = P.shape
print(f"日 {D}  格子 {G}  銘柄 {N}  {days[0]}〜{days[-1]}")
R = np.full((D, G, N), np.nan, np.float32)
R[:, 1:30, :] = np.log(P[:, 1:30, :] / P[:, 0:29, :])
R[:, 31:, :] = np.log(P[:, 31:, :] / P[:, 30:-1, :])
R[~np.isfinite(R)] = np.nan
# 市場（同時刻の断面平均）を抜いた残差
mkt = np.nanmean(R, axis=2, keepdims=True)
X = R - mkt
X[np.isnan(X)] = 0.0
valid = np.isfinite(R).all(axis=2)          # その (日, 格子) が全銘柄そろっている
print(f"リターンの標準偏差 中央値 {np.nanstd(R, axis=(0,1)).mean()*1e4:.1f} bp（残差 {X.std(axis=(0,1)).mean()*1e4:.1f} bp）")

def lagcorr(A, B, lag, dsel):
    """A[t] と B[t+lag] の相関行列（N×N）。同じ日・同じ場の中だけ。"""
    a, b = [], []
    for s, e in [(1, 30), (31, G)]:
        if e - s <= lag: continue
        m = valid[dsel][:, s:e - lag] & valid[dsel][:, s + lag:e]
        a.append(A[dsel][:, s:e - lag, :][m]); b.append(B[dsel][:, s + lag:e, :][m])
    a = np.concatenate(a); b = np.concatenate(b)
    a = (a - a.mean(0)) / (a.std(0) + 1e-12); b = (b - b.mean(0)) / (b.std(0) + 1e-12)
    return (a.T @ b) / len(a), len(a)

allday = np.ones(D, bool); h1 = np.zeros(D, bool); h1[:D // 2] = True; h2 = ~h1
print("\n== 残差リターンの lag 別 相関（全期間、対角=自己相関、非対角=銘柄間）")
Cs = {}
for lag in [0, 1, 2, 3]:
    C, n = lagcorr(X, X, lag, allday)
    Cs[lag] = C
    off = C[~np.eye(N, dtype=bool)]
    print(f"  lag {lag*5:2d} 分: n={n:6d}  自己相関 中央値 {np.median(np.diag(C)):+.4f}  "
          f"銘柄間 |ρ| の 中央値 {np.median(np.abs(off)):.4f} / 99%点 {np.quantile(np.abs(off), 0.99):.4f} / 最大 {np.abs(off).max():.4f}")
print("\n== 前半で選んだ組は後半でも残るか（lag 1〜3、上位 200 組の |ρ|）")
rows = []
for lag in [1, 2, 3]:
    C1, _ = lagcorr(X, X, lag, h1); C2, _ = lagcorr(X, X, lag, h2)
    m = ~np.eye(N, dtype=bool)
    i1 = np.argsort(-np.abs(C1 * m), axis=None)[:200]
    a1 = C1.flat[i1]; a2 = C2.flat[i1]
    keep_sign = (np.sign(a1) == np.sign(a2)).mean()
    ic = np.corrcoef(C1[m], C2[m])[0, 1]
    print(f"  lag {lag*5:2d} 分: 前半の上位 200 組 |ρ| 平均 {np.abs(a1).mean():.4f} → 後半 {np.abs(a2).mean():.4f}（符号一致 {keep_sign:.2f}）  全組の前半 vs 後半 ρ の相関 {ic:+.3f}")
    rows.append(dict(lag=lag * 5, h1=np.abs(a1).mean(), h2=np.abs(a2).mean(), sign=keep_sign, ic=ic))
    if lag == 2:
        ii, jj = np.unravel_index(i1[:8], C1.shape)
        print("     上位 8 組（A→B、前半 ρ / 後半 ρ）:", ', '.join(f"{codes[i]}→{codes[j]} {C1[i,j]:+.3f}/{C2[i,j]:+.3f}" for i, j in zip(ii, jj)))
# 経済的な大きさ: 残差 σ × ρ = 予測できる bp
sd = X.std(axis=(0, 1)).mean() * 1e4
print(f"\n== 予測できる大きさ: 残差の 5 分 σ = {sd:.1f} bp。ρ=0.05 なら {0.05*sd:.2f} bp、ρ=0.10 なら {0.10*sd:.2f} bp。往復費用 15.7 bp")
# 市場を抜く前（生のリターン）の lag 相関 = 非同期取引の目安
Y = R.copy(); Y[np.isnan(Y)] = 0.0
for lag in [0, 1, 2]:
    C, n = lagcorr(Y, Y, lag, allday); off = C[~np.eye(N, dtype=bool)]
    print(f"  生のリターン lag {lag*5:2d} 分: 銘柄間 ρ 中央値 {np.median(off):+.4f}  自己相関 中央値 {np.median(np.diag(C)):+.4f}")
pd.DataFrame(rows).to_csv(f'{OUT}/p6_leadlag.csv', index=False)
