# 利益を代用有価証券（ETF 現物）の買い増しに回して複利で回したら、最終資産はいくらになるか。
# 建玉 = 資産 × 掛目 /（保証金率 0.33 × 余裕 1.3）。キャパ上限（1 注文 500 万 = 現行の 5 倍）で頭打ち。
import sys, os; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

BASE_TATE = 5_000_000        # 現行の建玉（ロング 300 万 + ショート 200 万）
MR, BUF = 0.33, 1.3          # 委託保証金率・余裕係数
ETF_COST = 0.001             # ETF 買い付けの片道費用（手数料・スプレッド）
TAX = 0.20315                # 申告分離課税（daytrade の実現益に毎年かかる。ETF の含み益は売るまで無税）
CAP = 5.0                    # 1 注文 500 万（2026-09-jp-daytrade-capacity の上限）
START = 2_700_000            # 出発点（現行の建玉を ETF で賄う額）

t = pd.read_csv(f'{OUT}/trades_10y.csv'); t['date'] = pd.to_datetime(t.date)
dtd = t.groupby('date').pnl.sum()            # 現行規模（建玉 500 万）の日次損益

def sim(px, cap=CAP, kakeme=0.8, start=START, d0=None, d1=None, tax=0.0):
    """px: ETF の調整後終値（None なら現金のまま積む）。月初に利益を全額 ETF へ回し、建玉を建て直す。"""
    if px is None:
        idx = dtd.index
    else:
        idx = px.index
    idx = idx[(idx >= (d0 or idx[0])) & (idx <= (d1 or idx[-1]))]
    need = BASE_TATE * MR * BUF / kakeme      # 建玉 1 単位に必要な資産
    shares = start / px.loc[idx[0]] if px is not None else 0.0
    cash = 0.0 if px is not None else start
    s = min(cash * kakeme / need if px is None else start * kakeme / need, cap)
    rows, calls = [], 0
    prev_m = None; prev_y = None; ypnl = 0.0; carry = 0.0; tax_paid = 0.0
    for d in idx:
        p = px.loc[d] if px is not None else 0.0
        if px is not None and (prev_m is None or (d.year, d.month) != prev_m):   # 月初: 利益を ETF に回して建て直す
            if cash > 0: shares += cash * (1 - ETF_COST) / p; cash = 0.0
            s = min(shares * p * kakeme / need, cap)
        elif px is None and (prev_m is None or (d.year, d.month) != prev_m):
            s = min(cash * kakeme / need, cap)
        if tax and prev_y is not None and d.year != prev_y:      # 年初に前年の実現益へ課税（損失は 3 年繰越の代わりに無期限で相殺）
            base_ = ypnl + carry
            if base_ > 0: pay = base_ * tax; carry = 0.0
            else: pay = 0.0; carry = base_
            tax_paid += pay
            if px is not None and pay > 0:                        # 現金が足りなければ ETF を売って納める
                if cash >= pay: cash -= pay
                else: shares -= (pay - cash) / p; cash = 0.0
            else: cash -= pay
            ypnl = 0.0
            if px is not None: s = min(shares * p * kakeme / need, cap)
        prev_y = d.year
        prev_m = (d.year, d.month)
        equity = (shares * p if px is not None else 0.0) + cash
        if px is not None and shares * p * kakeme < BASE_TATE * s * MR:          # 追証（掛目 80% の評価が必要保証金を割る）
            calls += 1; s = min(shares * p * kakeme / need, cap)
        pnl = dtd.get(d, 0.0) * s
        cash += pnl; ypnl += pnl
        rows.append((d, equity, s, pnl))
    df = pd.DataFrame(rows, columns=['date', 'equity', 'scale', 'pnl']).set_index('date')
    final = (shares * px.loc[idx[-1]] if px is not None else 0.0) + cash
    eq = df.equity.copy(); eq.iloc[-1] = final
    dd = (eq - eq.cummax()).min(); ddp = ((eq / eq.cummax() - 1).min()) * 100
    yrs = (idx[-1] - idx[0]).days / 365.25
    cagr = (final / start) ** (1 / yrs) - 1
    hit = df[df.scale >= cap - 1e-9]
    return dict(final=final, cagr=cagr * 100, dd=dd, ddp=ddp, yrs=yrs, calls=calls, tax=tax_paid,
                cap_date=hit.index[0].date() if len(hit) else None,
                etf_val=shares * px.loc[idx[-1]] if px is not None else 0.0,
                dt_sum=df.pnl.sum(), eq=eq, scale=df.scale)

c = con()
ETFS = {'1306 TOPIX': '13060', '1321 日経225': '13210', '1655 S&P500円': '16550',
        '1545 NASDAQ100円': '15450', '1554 全世界除く日本': '15540', '2559 全世界円': '25590'}
px = {k: jp_etf(c, v).close for k, v in ETFS.items()}

D0, D1 = pd.Timestamp('2017-09-28'), pd.Timestamp('2026-09-07')   # 1655 が上場している共通期間
print(f"=== 共通期間 {D0.date()}〜{D1.date()}  出発 {START:,.0f} 円  キャパ上限 ×{CAP:.0f}（1 注文 500 万）===")
print(f"{'担保':<20} {'最終資産':>16} {'CAGR':>8} {'最大DD':>14} {'DD%':>7} {'上限到達':>12} {'ETF評価':>15} {'daytrade累計':>15} {'追証':>5}")
rows = []
for k, p in px.items():
    if p.index[0] > D0: continue
    r = sim(p, d0=D0, d1=D1)
    rows.append((k, r))
    print(f"{k:<20} {r['final']:>16,.0f} {r['cagr']:>7.1f}% {r['dd']:>14,.0f} {r['ddp']:>6.1f}% {str(r['cap_date']):>12} {r['etf_val']:>15,.0f} {r['dt_sum']:>15,.0f} {r['calls']:>5}")
r = sim(None, d0=D0, d1=D1); rows.append(('現金のまま積む', r))
print(f"{'現金のまま積む':<18} {r['final']:>16,.0f} {r['cagr']:>7.1f}% {r['dd']:>14,.0f} {r['ddp']:>6.1f}% {str(r['cap_date']):>12} {0:>15,.0f} {r['dt_sum']:>15,.0f} {r['calls']:>5}")
base = dtd[(dtd.index >= D0) & (dtd.index <= D1)].sum()
print(f"{'複利なし（現行）':<18} {START + base:>16,.0f} {'':>8} {'':>14} {'':>7} {'':>12} {'':>15} {base:>15,.0f}")

print(f"\n=== 全期間 2017-01-06〜2026-09-07（1306 / 1321 / 1545 / 1554）===")
for k in ['1306 TOPIX', '1321 日経225', '1545 NASDAQ100円', '1554 全世界除く日本']:
    r = sim(px[k], d0=pd.Timestamp('2017-01-06'))
    print(f"{k:<20} {r['final']:>16,.0f} {r['cagr']:>7.1f}% DD {r['dd']:>12,.0f} ({r['ddp']:.1f}%) 上限到達 {r['cap_date']}  ETF {r['etf_val']:,.0f}  daytrade {r['dt_sum']:,.0f}")
r = sim(None, d0=pd.Timestamp('2017-01-06'))
print(f"{'現金のまま積む':<18} {r['final']:>16,.0f} {r['cagr']:>7.1f}% DD {r['dd']:>12,.0f} ({r['ddp']:.1f}%) 上限到達 {r['cap_date']}")
print(f"{'複利なし（現行）':<18} {START + dtd.sum():>16,.0f}  （daytrade 累計 {dtd.sum():,.0f}）")

print(f"\n=== キャパ上限を変えたら（1655、{D0.date()}〜）===")
for cap in [1.0, 2.0, 3.0, 5.0, 10.0, 1e9]:
    r = sim(px['1655 S&P500円'], cap=cap, d0=D0, d1=D1)
    print(f"上限 ×{cap:>9,.0f}: 最終 {r['final']:>18,.0f} 円  CAGR {r['cagr']:6.1f}%  最大DD {r['dd']:>15,.0f} ({r['ddp']:.1f}%)  上限到達 {r['cap_date']}")

print(f"\n=== 余裕係数を厚くしたら（1655、上限 ×5、{D0.date()}〜）===")
for buf in [1.3, 1.5, 2.0, 3.0]:
    BUF = buf; r = sim(px['1655 S&P500円'], d0=D0, d1=D1)
    print(f"余裕 ×{buf}: 最終 {r['final']:>18,.0f} 円  CAGR {r['cagr']:6.1f}%  最大DD {r['dd']:>15,.0f} ({r['ddp']:.1f}%)  追証 {r['calls']} 日  上限到達 {r['cap_date']}")
BUF = 1.3

r = sim(px['1655 S&P500円'], d0=D0, d1=D1)
print(f"\n=== 1655・上限 ×5 の年末資産 ===")
y = r['eq'].resample('YE').last()
for d, v in y.items(): print(f"  {d.year}: {v:>18,.0f} 円  （建玉倍率 {r['scale'].resample('YE').last().loc[d]:.2f}）")
pd.DataFrame({'equity': r['eq'], 'scale': r['scale']}).to_csv(f'{OUT}/dt_compound.csv')

print(f"\n=== 税（申告分離 20.315%）を引いたら（上限 ×5、{D0.date()}〜）===")
for k in ['1306 TOPIX', '1655 S&P500円', '1545 NASDAQ100円']:
    a = sim(px[k], d0=D0, d1=D1); b = sim(px[k], d0=D0, d1=D1, tax=TAX)
    print(f"{k:<20} 税引前 {a['final']:>15,.0f} → 税引後 {b['final']:>15,.0f} 円（CAGR {b['cagr']:.1f}%、納税 {b['tax']:,.0f} 円、上限到達 {b['cap_date']}）")
b = sim(None, d0=D0, d1=D1, tax=TAX)
print(f"{'現金のまま積む':<18} 税引前 {sim(None, d0=D0, d1=D1)['final']:>15,.0f} → 税引後 {b['final']:>15,.0f} 円（CAGR {b['cagr']:.1f}%、納税 {b['tax']:,.0f} 円）")
