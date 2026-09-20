"""test/dt_wf_target.py の結果を事前登録の基準で集計する。

  test/.venv/bin/python test/dt_wf_target_report.py [tag]

根拠・事前登録: vault 20-research/2026-09-jp-daytrade-ml-target-magnitude.md
"""

import sys

import numpy as np
import pandas as pd

HALF = pd.Timestamp("2023-03-17")


def stats(x):
    """日次リターンの列から bp/日・t・Sharpe・勝率・最大 DD（単利の累積、%）。"""
    cum = x.cumsum()
    return dict(bp=x.mean() * 1e4, t=x.mean() / (x.std(ddof=1) / np.sqrt(len(x))), sharpe=x.mean() / x.std(ddof=1) * np.sqrt(250),
                win=(x > 0).mean() * 100, dd=(cum.cummax() - cum).max() * 100)


def tstat(x):
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))


def main():
    tag = sys.argv[1] if len(sys.argv) > 1 else "main"
    daily = pd.read_parquet(f"test/out/dt_wf_target_{tag}_daily.parquet")
    picks = pd.read_parquet(f"test/out/dt_wf_target_{tag}_picks.parquet")
    all_days = pd.DatetimeIndex(sorted(daily["d"].unique()))
    variants = [v for v in ["gap_vol", "base", "t1", "t1p", "t2", "t1_c3", "t1_c10"] if v in set(daily["variant"])]

    def series(v, col, seed=None):
        q = daily[daily["variant"] == v]
        if seed is not None:
            q = q[q["seed"] == seed]
        return q.groupby("d")[col].mean().reindex(all_days).fillna(0.0)

    def section(title, days, cols=("ret_liq", "ret_flat")):
        print(f"\n## {title}（{len(days):,} 日）")
        for col in cols:
            print(f"\n### {'流動性別コスト' if col == 'ret_liq' else '一律 5.7bp'}")
            print("| 形 | bp/日 | t | Sharpe | 勝率 | 最大 DD | base との差 | t | 前半 / 後半の差 | 差 > 0 のシード |")
            print("|---|---|---|---|---|---|---|---|---|---|")
            b = series("base", col)[days]
            for v in variants:
                x = series(v, col)[days]
                s = stats(x)
                row = f"| {v} | {s['bp']:+.2f} | {s['t']:.2f} | {s['sharpe']:.2f} | {s['win']:.1f}% | {s['dd']:.1f}% |"
                if v in ("base",):
                    print(row + " — | — | — | — |")
                    continue
                diff = x - b
                h1, h2 = diff[diff.index < HALF], diff[diff.index >= HALF]
                seeds = sorted(daily.loc[daily["variant"] == v, "seed"].unique())
                pos = sum((series(v, col, s_)[days] - series("base", col, s_ if v != "gap_vol" else None)[days]).mean() > 0 for s_ in seeds)
                print(row + f" {diff.mean() * 1e4:+.2f} | {tstat(diff):.2f} | {h1.mean() * 1e4:+.2f} / {h2.mean() * 1e4:+.2f} | {pos}/{len(seeds)} |")

    no_dec = all_days[all_days.month != 12]
    section("判定: 2019-09-17〜2026-09-16、12 月を除く", no_dec)
    section("再現の確認: 2021-09-17〜2026-09-16、12 月込み", all_days[all_days >= "2021-09-17"], cols=("ret_liq",))

    print("\n## 選定銘柄の性質（12 月を除く）")
    print("| 形 | ギャップ平均 | −5% 以下の割合 | −3% 以下の割合 | 売買代金の中央値（億） | 既存規則の順位の中央値 | 1 取引の平均 bp（コスト前） |")
    print("|---|---|---|---|---|---|---|")
    p = picks[picks["d"].dt.month != 12]
    for v in variants:
        q = p[p["variant"] == v]
        print(f"| {v} | {q['gap'].mean() * 100:.2f}% | {(q['gap'] <= -0.05).mean() * 100:.1f}% | {(q['gap'] <= -0.03).mean() * 100:.1f}% |"
              f" {q['turnover_med'].median() / 1e8:.1f} | {q['rule_rank'].median():.0f} | {q['y_raw'].mean() * 1e4:+.1f} |")

    trees = pd.read_csv(f"test/out/dt_wf_target_{tag}_trees.csv")
    print("\n## early stopping で選ばれた木の本数（シードの中央値）")
    print(trees.groupby(["variant", "fold"])["trees"].median().unstack().to_string())


if __name__ == "__main__":
    main()
