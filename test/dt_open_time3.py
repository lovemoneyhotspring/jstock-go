# 段階 5: 決定時刻 T に「T までに寄っている銘柄」を買い、引けで売る。帯 × T の実測。
# 建値は T までの最後の約定値（寄値ではない——既に寄った銘柄は反発を払って買う）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

TIMES = ['09:01', '09:03', '09:05', '09:07', '09:10', '09:15']
COST = 5.7   # 往復（信用買い 0 円 + 金利 + 滑り 5bp）
c = con()
df = c.execute(f"""
  SELECT g.Date, g.Code, g.gap*100 AS gap_pct, g.open_time, g.vol20,
         b.C AS close_raw, {', '.join('p.px_' + t.replace(':','') for t in TIMES)}
  FROM read_parquet('{OUT}/open_gap.parquet') g
  JOIN read_parquet('{OUT}/open_px_at.parquet') p ON p.Date = g.Date AND p.Code = g.Code
  JOIN bars b ON b.Date = g.Date AND b.Code = g.Code
  WHERE g.gap < 0 AND b.C > 0""").df()

BANDS = [-100, -5, -4, -3, -2, -1.5, -1, -0.5, 0]
LAB = ['≤−5%', '−5〜−4', '−4〜−3', '−3〜−2', '−2〜−1.5', '−1.5〜−1', '−1〜−0.5', '−0.5〜0']
df['帯'] = pd.cut(df.gap_pct, BANDS, labels=LAB)
days = df.Date.nunique()

print(f"495 営業日 / 対象 {len(df):,} 件。数字は「その時刻に成行で買い、引けで売る」費用込み net bp\n")
out = {}
for t in TIMES:
    col = 'px_' + t.replace(':', '')
    ok = df[df.open_time <= t].dropna(subset=[col])
    r = (ok.close_raw / ok[col] - 1) * 1e4 - COST
    s = r.groupby(ok['帯'], observed=True).agg(['mean', 'size'])
    out[t] = s
tbl = pd.DataFrame({t: out[t]['mean'].round(1) for t in TIMES})
cnt = pd.DataFrame({t: (out[t]['size'] / days).round(1) for t in TIMES})
print("■ net bp（帯 × 決定時刻）"); print(tbl.to_string())
print("\n■ 1 日あたり買える銘柄数"); print(cnt.to_string())
