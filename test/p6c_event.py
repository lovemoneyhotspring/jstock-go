# 事象ベースの追従: 同時相関の高い相棒（上位 5）を持つ A が 5 分で 3σ 動いた直後、B は次の 5/10/15 分で追いつくか。
# 「B がまだ動いていない」場合（乖離が開いた瞬間）に絞った変種も見る。前半・後半で分ける。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import json
P = np.load(f'{OUT}/p6_prices.npy'); meta = json.load(open(f'{OUT}/p6_meta.json'))
codes = meta['codes']; days = np.array(meta['files']); D, G, N = P.shape
R = np.full((D, G, N), np.nan, np.float32)
R[:, 1:30, :] = np.log(P[:, 1:30, :] / P[:, 0:29, :]); R[:, 31:, :] = np.log(P[:, 31:, :] / P[:, 30:-1, :])
R[~np.isfinite(R)] = np.nan
X = R - np.nanmean(R, axis=2, keepdims=True)
valid = np.isfinite(X).all(axis=2); X[np.isnan(X)] = 0.0
sd = X.std(axis=(0, 1))
# 同時相関
def flat(sel):
    a = []
    for s, e in [(1, 30), (31, G)]: a.append(X[sel][:, s:e, :][valid[sel][:, s:e]])
    return np.concatenate(a)
Z = flat(np.ones(D, bool)); Zn = (Z - Z.mean(0)) / (Z.std(0) + 1e-12)
C0 = (Zn.T @ Zn) / len(Zn); np.fill_diagonal(C0, -9)
part = np.argsort(-C0, axis=1)[:, :5]           # 各 A の相棒 5 銘柄
print(f"同時相関: 相棒 5 の ρ 平均 {np.take_along_axis(C0, part, 1).mean():.3f}  上位組 {np.take_along_axis(C0, part, 1).max():.3f}")
h1 = days < '2025-09-01'
rows = []
print("\n== A が 5 分で 3σ 動いた直後の B（相棒）の残差リターン。正 = 追従、負 = 逆行。bp（括弧 t、n）")
for lagged_only in [False, True]:
    for hname, dsel in [('前半', h1), ('後半', ~h1)]:
        acc = {1: [], 2: [], 3: []}; raw = {1: [], 2: [], 3: []}; betas = []
        for s, e in [(1, 30), (31, G)]:
            xa = X[dsel][:, s:e, :]; va = valid[dsel][:, s:e]
            for k in range(3):
                nb = X[dsel][:, s + 1 + k:e + 1 + k, :] if e + 1 + k <= G else None
            for a in range(N):
                ev = (np.abs(xa[:, :, a]) >= 3 * sd[a]) & va
                if not ev.any(): continue
                di, gi = np.nonzero(ev)
                sgn = np.sign(xa[:, :, a][ev])
                for b in part[a]:
                    beta = C0[a, b] * sd[b] / sd[a]
                    if lagged_only:
                        exp_b = beta * xa[:, :, a][ev]; act_b = xa[:, :, b][ev]
                        m = np.abs(act_b) < 0.3 * np.abs(exp_b)      # B がまだ追いついていない
                    else:
                        m = np.ones(len(di), bool)
                    for k, key in [(1, 1), (2, 2), (3, 3)]:
                        gj = gi + k; okk = m & (gj < e - s)
                        if not okk.any(): continue
                        # k 本ぶんの累積
                        cum = np.zeros(okk.sum())
                        for q in range(1, k + 1):
                            gq = gi[okk] + q
                            good = gq < e - s
                            cum[good] += X[dsel][:, s:e, b][di[okk][good], gq[good]]
                        acc[key].append(cum * sgn[okk] / (beta if beta != 0 else 1) * np.sign(beta))
                        raw[key].append(cum * sgn[okk] * np.sign(beta)); betas.append(abs(beta))
        line = f"  {'B が未追随のみ' if lagged_only else '全事象      '} {hname}:"
        for k in [1, 2, 3]:
            v = np.concatenate(acc[k]) if acc[k] else np.array([0.0])
            w = np.concatenate(raw[k]) if raw[k] else np.array([0.0])
            t = w.mean() / w.std() * np.sqrt(len(w)) if w.std() > 0 else np.nan
            line += f"  +{k*5:2d}分 実 {w.mean()*1e4:+5.2f}bp (t{t:+5.1f}) β正規化 {v.mean()*1e4:+5.2f}bp n={len(w):6d}"
            rows.append(dict(lagged_only=lagged_only, half=hname, ahead=k * 5, bp_raw=w.mean() * 1e4, bp_norm=v.mean() * 1e4, t=t, n=len(w)))
        line += f"  |β| 平均 {np.mean(betas):.2f}"
        print(line, flush=True)
pd.DataFrame(rows).to_csv(f'{OUT}/p6_event.csv', index=False)
