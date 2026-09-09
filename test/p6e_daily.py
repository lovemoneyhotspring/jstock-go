# 日足での先行・追従: A の当日の残差リターンが B の翌日を予測するか。5 分では費用に負けるので、σ が大きい日次なら残るかを見る。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
c = con()
d = c.execute("""
WITH liq AS (SELECT Code FROM (SELECT Code, median(TRY_CAST(Va AS DOUBLE)) va, count(*) n FROM bars WHERE Date >= '2017-01-01' GROUP BY Code) WHERE va >= 1e9 AND n >= 2200)
SELECT b.Date, b.Code, b.AdjC FROM bars b JOIN liq USING (Code) WHERE b.Date >= '2017-01-01' AND b.AdjC > 0""").df()
d['Date'] = pd.to_datetime(d.Date)
p = d.pivot(index='Date', columns='Code', values='AdjC').dropna(axis=1, thresh=int(0.98 * d.Date.nunique())).ffill()
r = np.log(p / p.shift(1)).iloc[1:]
X = (r.sub(r.mean(axis=1), axis=0)).fillna(0).values.astype(np.float32)
T, N = X.shape
print(f"日 {T}  銘柄 {N}  {p.index[1].date()}〜{p.index[-1].date()}  残差の日次 σ 中央値 {np.median(X.std(0))*1e4:.0f} bp")
def lc(A, B, lag):
    a = A[:len(A) - lag] if lag else A; b = B[lag:]
    a = (a - a.mean(0)) / (a.std(0) + 1e-12); b = (b - b.mean(0)) / (b.std(0) + 1e-12)
    return (a.T @ b) / len(a)
m = ~np.eye(N, dtype=bool)
for lag in [0, 1, 2]:
    C = lc(X, X, lag); off = C[m]
    print(f"  lag {lag} 日: 自己相関 中央値 {np.median(np.diag(C)):+.4f}  銘柄間 |ρ| 中央値 {np.median(np.abs(off)):.4f} / 99%点 {np.quantile(np.abs(off),0.99):.4f} / 最大 {np.abs(off).max():.4f}  （雑音 {1/np.sqrt(len(X)):.4f}）")
h = T // 2
for lag in [1, 2]:
    C1 = lc(X[:h], X[:h], lag); C2 = lc(X[h:], X[h:], lag)
    i1 = np.argsort(-np.abs(C1 * m), axis=None)[:200]
    a1, a2 = C1.flat[i1], C2.flat[i1]
    print(f"  lag {lag} 日: 前半 上位 200 組 |ρ| {np.abs(a1).mean():.4f} → 後半 {np.abs(a2).mean():.4f}（符号一致 {(np.sign(a1)==np.sign(a2)).mean():.2f}）  全組 前半 vs 後半 {np.corrcoef(C1[m], C2[m])[0,1]:+.3f}")
sd = np.median(X.std(0)) * 1e4
print(f"  予測できる大きさ: 残差の日次 σ = {sd:.0f} bp。ρ=0.05 なら {0.05*sd:.1f} bp、ρ=0.10 なら {0.10*sd:.1f} bp。往復費用 15.7 bp")
