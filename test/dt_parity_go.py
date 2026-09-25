"""Python の簡易検証（誤差なしの本番の形）と Go のバックテストの突き合わせ。どの層でずれるかを出す。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md（ストキャス RSI の優先で Python と Go の向きが逆になった件）

  bin/daytrade backtest --config-dir config/daytrade_margin --since 2017-01-01 --trades-csv test/out/parity_go_trades.csv
  PYTHONPATH=test test/.venv/bin/python test/dt_parity_go.py [--go test/out/parity_go_trades.csv] [--no-afford | --approx] [--us nasdaq] [--keep-december]

簡易検証で案を測るときは、先にこれを base（案なし）で回して、比べる日の層でずれが小さいことを確かめる。
ずれが大きい層（日の種類）の結果は、Python の数字を判定に使わない。

層:
  1 日     Python の日の区分（day_rules: 休み・ショック・平常）と Go のその日の並べ方（trades の rank_by）の対応。
          --us spx（既定・本番と同じ判定）なら小幅高の日は Go の lgbm の日と一致する
  2 銘柄   Go が gap_vol で並べた日に、Go の建てた銘柄と Python の上位 n 本（n = Go のその日の本数、誤差なし・業種 1・
          単元の判定あり）の重なり。Go が lgbm で並べた日は Python の gap_vol の模擬では表せないので比べない
  3 値     同じ日・銘柄で、Go の建値→手仕舞い値の騰落と候補表の y_raw（始値→引け）の差
  4 損益   日次の bp（等金額・コスト 5.7bp）で、Python の選定・Go の選定×候補表の値・Go の実損益（金額加重）を比べる
          「Python の選定 → Go の選定」の差は選定の違い、「Go の選定 → Go の実損益」の差は値・配分・費用の違い
"""

import argparse
import os

import numpy as np
import pandas as pd

from dt_nscale import COST, IS_END, SINCE, day_rules, seen_ranked, tstat

CAND = "test/out/dt_candidates_wide.parquet"
TOTAL = 7.0e6        # ロングの総額（max_capital 500 万 + 止めたショートの枠 200 万。config/daytrade_margin）
NAME_DIVISOR = 7     # capital.name_divisor
TURNOVER_RATIO = 0.002


def unit_affordable(mult):
    """Go の affordable（規則 R）: 1 単元（100 株）が min(売買代金 × 0.2%, 総額 ÷ 7) に載るか。
    mult は日 → 資金の倍率（ショック日 1.5）。"""
    def f(g):
        cap = TOTAL * g["d"].map(mult).fillna(1.0).values / NAME_DIVISOR
        return (g["price"].values * 100 <= np.minimum(TURNOVER_RATIO * g["turnover_med"].values, cap)).tolist()
    return f


def go_rank_by(path="test/out/parity_go_trades.csv"):
    """Go のバックテストが日ごとにロングを並べた方法（d → "gap_vol" / "lgbm"）。
    Python の簡易検証で日の区分を day_rules の近似でなく Go から取るときに使う（lgbm の日 = Go の米国小幅高の日）。"""
    go = pd.read_csv(path, dtype={"code": str}, parse_dates=["date"], usecols=["date", "side", "rank_by"])
    go = go[go["side"] == "long"]
    return go.groupby("date")["rank_by"].first().rename_axis("d")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--go", default="test/out/parity_go_trades.csv")
    ap.add_argument("--no-afford", action="store_true", help="単元の判定を掛けない（従来の Python の形）")
    ap.add_argument("--approx", action="store_true",
                    help="Python の選定を alloc_rule（DT_AFFORD=1 の近似: 業種の上限の後で単元の判定）で作る。"
                         "既定は seen_ranked の afford（Go と同じ順）で上位 n 本")
    ap.add_argument("--keep-december", action="store_true", help="12 月も取引する（従来の Python の形）")
    ap.add_argument("--us", default="spx", choices=["spx", "nasdaq"], help="day_rules の米国小幅高の判定（nasdaq は従来の近似）")
    a = ap.parse_args()

    go = pd.read_csv(a.go, dtype={"code": str}, parse_dates=["date"])
    go = go[go["side"] == "long"].rename(columns={"date": "d"})
    go["ret_go"] = go["exit"] / go["entry"] - 1

    te = pd.read_parquet(CAND)
    te = te[(te["d"] >= SINCE) & (te["d"] <= go["d"].max())].copy()
    te["code"] = te["code"].astype(str)
    from dt_preopen_sim import BANDS
    rules = day_rules(te, us=a.us, skip_months=() if a.keep_december else (12,))
    afford = None if a.no_afford else unit_affordable(rules["mult"])
    if a.approx:
        os.environ["DT_AFFORD"] = "1"
        afford = None
    g = seen_ranked(te, [np.zeros(1)] * (len(BANDS) - 1), 0, afford=afford)  # 誤差なし
    print(f"単元の判定: {'なし' if afford is None else 'あり（総額 %.0f 万 ÷ %d、売買代金 × %.1f%%）' % (TOTAL / 1e4, NAME_DIVISOR, TURNOVER_RATIO * 100)}")

    kind = pd.Series(np.where(rules["mult"] == 0, "休む月（12 月）", np.where(rules["skip"], "小幅高（Python は休み）",
                              np.where(rules["mult"] > 1, "ショック", "平常"))), index=rules.index)
    alld = rules.index
    n_go = go.groupby("d").size().reindex(alld).fillna(0).astype(int)

    rb = go_rank_by(a.go).reindex(alld).fillna("建てない")

    # 層 1: 日
    print("## 層 1: 日（行 = Python の区分、列 = Go のその日の並べ方。日数）")
    print(pd.crosstab(kind, rb).to_string())

    # 層 2: 銘柄（Go が gap_vol で並べた日だけ）
    days = alld[(rb == "gap_vol").values]
    if a.approx:
        from dt_nscale import alloc_rule
        idx = [i for d, x in g[g["d"].isin(days)].groupby("d")
               for i in alloc_rule(x, TURNOVER_RATIO, 10, TOTAL * rules.loc[d, "mult"], k=NAME_DIVISOR)[0]]
        py = g.loc[idx]
    else:
        py = g[g["d"].isin(days)].merge(n_go.rename("n"), left_on="d", right_index=True)
        py = py[py["rank"] <= py["n"]]
    gs = go[go["d"].isin(days)]
    both = py.merge(gs[["d", "code"]], on=["d", "code"])
    jac = (both.groupby("d").size().reindex(days).fillna(0)
           / (py.groupby("d").size().reindex(days).fillna(0) + gs.groupby("d").size().reindex(days).fillna(0)
              - both.groupby("d").size().reindex(days).fillna(0)))
    only_go = gs.merge(py[["d", "code"]], on=["d", "code"], how="left", indicator=True)
    only_go = only_go[only_go["_merge"] == "left_only"]
    in_cand = only_go.merge(te[["d", "code"]], on=["d", "code"], how="inner")
    print(f"\n## 層 2: 銘柄（Go が gap_vol で並べた {len(days)} 日）")
    print(f"- 日ごとの一致（Jaccard）: 平均 {jac.mean():.3f}、完全一致の日 {(jac == 1).mean():.1%}")
    print(f"- Go だけが建てた {len(only_go):,} 件のうち、候補表に無い銘柄 {len(only_go) - len(in_cand):,} 件"
          f"（母集団の違い）、候補表にあるが Python の上位 n 本に入らない {len(in_cand):,} 件（順位の違い）")

    # 層 3: 値
    v = gs.merge(te[["d", "code", "y_raw", "o"]], on=["d", "code"])
    v["diff_bp"] = (v["ret_go"] - v["y_raw"]) * 1e4
    print(f"\n## 層 3: 値（同じ日・銘柄 {len(v):,} 件）")
    print(f"- 騰落の差の絶対値: 中央値 {v['diff_bp'].abs().median():.2f} bp、1 bp 超 {(v['diff_bp'].abs() > 1).mean():.1%}、"
          f"平均 {v['diff_bp'].mean():+.2f} bp")
    print(f"- 建値と候補表の始値が違う: {(~np.isclose(v['entry'], v['o'])).mean():.1%}、持ち越し（張り付き）"
          f" {gs['carried'].mean():.2%}")

    # 層 4: 損益（bp/日）
    s_py = (py.assign(net=(py["y_raw"] - COST) * 1e4).groupby("d")["net"].mean())
    gv = gs.merge(te[["d", "code", "y_raw"]], on=["d", "code"], how="left")
    s_gy = gv.assign(net=(gv["y_raw"] - COST) * 1e4).groupby("d")["net"].mean()
    s_go = gs.groupby("d").apply(lambda x: x["pnl"].sum() / x["amount"].sum() * 1e4, include_groups=False)
    tab = pd.DataFrame({"Python の選定": s_py, "Go の選定×候補表の値": s_gy, "Go の実損益": s_go}).reindex(days)
    print("\n## 層 4: 損益（bp/日、Go が gap_vol で並べた日）")
    print("| 期間 | " + " | ".join(tab.columns) + " | 選定の差（t） | 値・配分・費用の差（t） | 日次の相関 |")
    print("|---|" + "---|" * (len(tab.columns) + 3))
    for name, m in [("IS 〜2021", tab.index <= IS_END), ("OOS 2022〜", tab.index > IS_END), ("全期間", tab.index == tab.index)]:
        t = tab[m]
        d1 = (t.iloc[:, 1] - t.iloc[:, 0]).dropna()
        d2 = (t.iloc[:, 2] - t.iloc[:, 1]).dropna()
        print(f"| {name} | " + " | ".join(f"{t[c].mean():+.2f}" for c in tab.columns)
              + f" | {d1.mean():+.2f}（{tstat(d1):+.2f}） | {d2.mean():+.2f}（{tstat(d2):+.2f}）"
              + f" | {t.iloc[:, 0].corr(t.iloc[:, 2]):.3f} |")

    out = "test/out/dt_parity_go_days.csv"
    tab.assign(kind=kind.reindex(days), jaccard=jac).to_csv(out)
    print(f"\n日ごとの表: {out}")


if __name__ == "__main__":
    main()
