"""公募増資の P1（発表後の寄りで売り → 条件決定の日の引けで買い戻す）が、実際に売れたか・逆日歩でいくら削られたか。

根拠: vault 20-research/2026-09-jp-public-offering.md の「次 1」。前向きに待つ代わりに、日証金の履歴（2023-04〜）で確かめる。
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/po_jsf_check.py > test/out/po_jsf_check.txt 2>&1

■ 事象
  po_offering.py の出力（test/out/po_offering.csv）の公募増資のうち、P1 を測れて、発表時点で貸借銘柄（shortable）、
  入口の日が 2023-04-03 以降のもの。
■ 日証金から引くもの（jsf_lending.fetch、申込日ごと）
  入口の日から条件決定の日（P1 の出口の日）までの各営業日の、制限措置（停・注）と品貸料率の年率換算（%）。
■ 実際に売れたか
  入口の日に「停」（申込停止）なら、制度信用で新規売りできない → その事象は建てない（立花は一般信用で新規売りできない）。
■ 逆日歩
  逆日歩のコスト = 保有した各営業日の品貸料率の年率換算の平均 × 入口の日から出口の日までの暦日数 / 365。
  （品貸料は受渡の暦日数でかかる。この近似は 1〜2 日ずれうる）
  P1 の実額 = po_offering.csv の p1 − 逆日歩のコスト（貸株料 年 3.1% はもとの p1 に入っている）。
■ 採否（実運用に進めるか）
  1: 入口の日に申込停止だった事象が半分未満
  2: 建てられた事象の P1 の実額の平均 > 0
  3: 建てられた事象の逆日歩のコストの平均が、同じ事象の P1 のコスト前の超過（−p1_ex）の平均の半分未満
  すべて満たせば、実運用の形（1 件の金額・同時に持つ数・発表の拾い方）を事前登録する。
  件数が少ない（数十件）ので t は報告だけ。
■ 報告だけ
  停・注が付いた日（入口の何日後か）、注意喚起の割合、逆日歩の分布（中央値・最大）、最高料率、
  入口の日の後で停止になった事象（建てた後に停止になっても、既存の建玉は返済できる）。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from common import *  # noqa: E402,F403
from jsf_lending import fetch  # noqa: E402

START = pd.Timestamp('2023-04-03')


def main():
    c = con()
    tdays = topix(c).index
    d = pd.read_csv(f'{OUT}/po_offering.csv', parse_dates=['ann', 'entry', 'price_at', 'p2_entry'])
    d = d[(d.kind == 'po') & d.p1.notna() & d.shortable.astype(bool) & (d.entry >= START)].copy()
    print(f'対象 {len(d)} 件', flush=True)
    rows = []
    for _, r in d.iterrows():
        a = int(np.searchsorted(tdays, r.entry))
        b = a + int(r.days_to_price) - 1
        days = tdays[a:b + 1]
        recs = [fetch(r.code, str(x.date())) for x in days]
        flags = [x.get('flag', '') if x.get('found') else '?' for x in recs]
        fees = [x.get('fee_pct') for x in recs if x.get('found') and x.get('fee_pct') is not None]
        cal = (days[-1] - days[0]).days + 1
        fee_cost = (np.mean(fees) if fees else 0.0) / 100 * cal / 365
        rows.append(dict(code=r.code, entry=r.entry.date(), n_days=len(days), flags=''.join(f or '-' for f in flags),
                         stop_entry=flags[0] == '停', found_entry=flags[0] != '?',
                         warn_any='注' in flags, stop_later='停' in flags[1:] and flags[0] != '停',
                         fee_mean=np.mean(fees) if fees else 0.0, fee_max=max(fees) if fees else 0.0,
                         max_rate=max((x.get('max_fee') or 0) for x in recs if x.get('found')) if any(x.get('found') for x in recs) else np.nan,
                         fee_cost_bp=fee_cost * 1e4, p1_bp=r.p1 * 1e4, p1_ex_bp=r.p1_ex * 1e4,
                         p1_real_bp=(r.p1 - fee_cost) * 1e4))
    x = pd.DataFrame(rows)
    x.to_csv(f'{OUT}/po_jsf_check.csv', index=False)
    pd.set_option('display.width', 220)
    print(x.to_string(index=False))
    ok = x[x.found_entry]
    print(f'\n日証金に行が無い事象: {int((~x.found_entry).sum())}')
    stop = ok.stop_entry.mean()
    t = ok[~ok.stop_entry]
    m, s = t.p1_real_bp.mean(), t.p1_real_bp.std(ddof=1)
    print(f'入口の日に申込停止: {int(ok.stop_entry.sum())}/{len(ok)}（{stop:.0%}）  注意喚起が付いた: {ok.warn_any.mean():.0%}  '
          f'建てた後に停止: {int(ok.stop_later.sum())}')
    print(f'建てられた {len(t)} 件: P1（貸株料 3.1% のみ）{t.p1_bp.mean():+.1f}  逆日歩 {t.fee_cost_bp.mean():.1f} bp'
          f'（中央値 {t.fee_cost_bp.median():.1f}・最大 {t.fee_cost_bp.max():.1f}）  P1 の実額 {m:+.1f}（t {m / s * np.sqrt(len(t)):+.2f}、'
          f'中央値 {t.p1_real_bp.median():+.1f}、勝率 {(t.p1_real_bp > 0).mean():.0%}）  コスト前の超過 {-t.p1_ex_bp.mean():+.1f}')
    c1, c2, c3 = stop < 0.5, m > 0, t.fee_cost_bp.mean() < -t.p1_ex_bp.mean() / 2
    print(f'判定: 1 {c1}  2 {c2}  3 {c3}  → {"実運用の形を事前登録する" if c1 and c2 and c3 else "実運用に進めない"}')
    st = ok[ok.stop_entry]
    if len(st):
        print(f'（報告）申込停止で建てられなかった {len(st)} 件の P1（仮に売れていたら）: {st.p1_bp.mean():+.1f}')


if __name__ == '__main__':
    main()
