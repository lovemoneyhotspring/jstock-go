"""JPX日経中小型株指数の採用（毎年 8 月）の予定を前もって知らせる（deploy/jsm-remind.sh から毎朝 7:30、7〜8 月）。

根拠: vault 30-projects/季節性イベント.md。人が別の証券会社で発注するため、前日に言われても間に合わない（ユーザ、2026-09-26）。

  test/.venv/bin/python test/jsm_remind.py [--date 2027-07-08]   # 今日（または --date の日）に送る知らせを標準出力に出す。無ければ何も出さない

予定の決め方（2017〜2026 の 9 回すべてで成り立った規則）:
  発表 = 8 月の 5 営業日目（引け後）。買う日 = その翌営業日（寄成）。
  実施日 = 8 月の最終営業日。売る日 = その前営業日（組み入れの日、引成）。
  営業日は J-Quants の取引カレンダー（cal、HolDiv='1'）。その年が無ければ土日だけを除いて数える（知らせにそう書く）。
知らせる日:
  発表の 30 日前（暦日）: 1 か月前のお知らせ。発表の 14〜1 日前: 毎朝のカウントダウン。
  発表の日・買う日・売る日の前営業日・売る日: その日にやること。
"""
import argparse
import os
import sys
from datetime import date

sys.path.insert(0, os.path.dirname(__file__))
from common import *  # noqa: E402,F403

BUDGET = float(os.environ.get('JSM_BUDGET_PER_NAME', 500000))
MAX_NAMES = int(os.environ.get('JSM_MAX_NAMES', 20))


def schedule(year, c=None):
    c = c or con()
    cal = c.execute(f"SELECT Date FROM cal WHERE HolDiv='1' AND Date BETWEEN '{year}-07-01' AND '{year}-09-30' ORDER BY Date").df()
    exact = len(cal) > 0
    if exact:
        b = pd.DatetimeIndex(pd.to_datetime(cal.Date))
    else:
        b = pd.bdate_range(f'{year}-07-01', f'{year}-09-30')
    aug = b[(b.month == 8)]
    ann = aug[4]
    entry = b[b > ann][0]
    eff = aug[-1]
    exit_ = b[b < eff][-1]
    prev = b[b < exit_][-1]
    return dict(ann=ann, entry=entry, eff=eff, exit=exit_, prev=prev, exact=exact)


WD = '月火水木金土日'


def md(t):
    return f'{t.month}/{t.day}（{WD[t.weekday()]}）'


def message(today, s):
    ann, entry, exit_ = s['ann'], s['entry'], s['exit']
    note = '' if s['exact'] else '（取引カレンダーがまだ無いので、土日だけを除いた予想）'
    dates = f'発表 {md(ann)} 引け後 → 買う {md(entry)} 寄成 → 売る {md(exit_)} 引成{note}'
    cap = MAX_NAMES * BUDGET
    days = (ann - today).days
    if days == 30:
        return ('中小型株指数の採用: 1 か月前のお知らせ',
                f'{ann.year} 年の JPX日経中小型株指数の定期入替まであと 1 か月。\n{dates}\n'
                f'買うのは売買代金の大きい順に {MAX_NAMES} 銘柄、1 銘柄 {BUDGET / 1e4:.0f} 万円で合計 約 {cap / 1e4:,.0f} 万円'
                '（1 単元が予算を超える銘柄は 1 単元）。\n準備: 別の証券会社の口座に資金を入れる／寄成・引成を 20 銘柄出せるか確かめる／'
                '8 月の買う日と売る日に注文を入れられるか予定を空ける。\n根拠と手順: vault 30-projects/季節性イベント.md')
    if 1 <= days <= 14:
        return (f'中小型株指数の採用: 発表まであと {days} 日',
                f'{dates}\n資金 約 {cap / 1e4:,.0f} 万円（{MAX_NAMES} 銘柄 × {BUDGET / 1e4:.0f} 万円）。'
                '発表の夜に注文一覧が Discord と vault（20-research/<年>-08-jsm-adds.md）に届く。')
    if today == ann:
        return ('中小型株指数の採用: 今夜発表',
                f'今日の引け後に発表の予定。17:30・19:30・21:30 に確かめ、見つけたら注文一覧を送る。\n'
                f'明日 {md(entry)} の寄成で買う（8:00 までに注文を入れる）。')
    if today == entry:
        return ('中小型株指数の採用: 今日の寄成で買う',
                f'注文一覧（vault 20-research/{ann.year}-08-jsm-adds.md）の {MAX_NAMES} 銘柄を寄成で買う。'
                '昨夜に一覧が届いていなければ、発表が予定と違う日だった可能性がある（JPX のページを確かめる）。\n'
                f'売るのは {md(exit_)} の引成。')
    prev = s['prev']
    if today == prev:
        return ('中小型株指数の採用: 次の営業日の引成で売る', f'次の営業日 {md(exit_)} の引成で全銘柄を売る（組み入れの日）。')
    if today == exit_:
        return ('中小型株指数の採用: 今日の引成で売る',
                '今日は組み入れの日。全銘柄を引成で売る（14:50 ごろまでに注文を入れる）。約定値は注文一覧の記入欄へ（任意）。')
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--date', default='')
    a = ap.parse_args()
    today = pd.Timestamp(a.date) if a.date else pd.Timestamp(date.today())
    m = message(today, schedule(today.year))
    if m:
        print(m[0])
        print(m[1])


if __name__ == '__main__':
    main()
