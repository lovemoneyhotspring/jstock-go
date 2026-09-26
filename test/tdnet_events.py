"""TDnet の表題から事象を拾う規則（公募増資・条件決定・市場区分の昇格）。株価は読まない。

  test/.venv/bin/python test/tdnet_events.py        # 件数の確認だけ

規則は po_offering.py・mkt_promotion.py の事前登録の一部。結果を見てから変えない。
"""
import os
import re

import pandas as pd

OUT = os.path.join(os.path.dirname(__file__), 'out')

EXCL_COMMON = r'訂正|投資口|中止|延期|取りやめ|取止め|資金使途|完了|結果|経過|変更に関する|数の変更|決議取消'
# 公募増資（売出しを伴う新株発行・自己株式の処分、または「公募」と書いたもの）
PO = r'(?:(?:新株式?発行|新株の発行|自己株式の処分).*(?:売出|売り出し)|公募)'
PO_EXCL = EXCL_COMMON + r'|^(?!.*(新株式?発行|新株の発行|自己株式の処分)).*社債|決定|第三者割当(?!.*公募)|新株予約権(?!付社債)|ストック・?オプション|株式報酬|給付信託|持株会'
# 株式の売出しだけ（希薄化なし）。報告だけに使う
SO = r'(?:株式の?売出|株式売出)'
SO_EXCL = EXCL_COMMON + r'|決定|新株式?発行|新株の発行|自己株式の処分|公募'
# 条件決定
PRICE = r'(?:発行価格|売出価格|処分価格).*決定'
PRICE_EXCL = r'訂正|投資口|第三者割当(?!.*公募)|新株予約権|ストック・?オプション'
# 市場区分の昇格（市場第一部・プライム市場へ）。申請の段階は含めない
PROMO = r'(?:市場第一部|市場第１部|東証一部|東証１部|プライム市場).*(?:指定|市場変更|区分変更|上場市場の変更|市場の変更)'
PROMO_EXCL = r'申請|訂正|記念|配当|基準|適合|計画|維持|経過措置|選択|株主優待|予定'


def load():
    df = pd.read_parquet(os.path.join(OUT, 'tdnet.parquet'))
    df['code'] = df.company_code.astype(str).str.strip()
    df = df[df.code.str.len() == 5]
    return df


def pick(df, pat, excl):
    m = df.title.str.contains(pat, regex=True) & ~df.title.str.contains(excl, regex=True)
    return df[m].sort_values('pubdate')


def events(df=None):
    df = load() if df is None else df
    po = pick(df, PO, PO_EXCL)
    so = pick(df, SO, SO_EXCL)
    pr = pick(df, PRICE, PRICE_EXCL)
    promo = pick(df, PROMO, PROMO_EXCL)
    # 同じ銘柄で 30 日以内の重複は最初の 1 件（公募は同じ案件の続報、昇格は承認と指定の二重）
    def first(x, days):
        x = x.sort_values('pubdate').copy()
        keep, last = [], {}
        for i, r in x.iterrows():
            p = last.get(r.code)
            if p is None or (r.pubdate - p).days > days:
                keep.append(i)
                last[r.code] = r.pubdate
        return x.loc[keep]
    return dict(po=first(po, 30), so=first(so, 30), price=pr, promo=first(promo, 60))


if __name__ == '__main__':
    ev = events()
    for k, v in ev.items():
        print(k, len(v), v.groupby(v.pubdate.dt.year).size().to_dict())
    for k in ('po', 'so', 'promo'):
        print(f'\n--- {k} の表題の例')
        print(ev[k].title.str.slice(0, 60).value_counts().head(12).to_string())
