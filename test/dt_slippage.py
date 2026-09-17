"""daytrade の本番の約定で滑りを測り、建玉を増やしたときの試算の前提（I0）と比べる。

根拠: vault 20-research/2026-09-jp-daytrade-margin-80pct-compound.md（上限 1500 万の判断は I0 次第）
      vault 20-research/2026-09-jp-daytrade-long-10m.md（注文が寄付の約定代金に占める割合）

  test/.venv/bin/python test/dt_slippage.py                 # 2026-09-18（LightGBM の初日）から
  test/.venv/bin/python test/dt_slippage.py --from 2026-10-01

測るもの（bp、不利なら正）:
  entry   約定単価と日足の始値の差。backtest は始値で建てるので、これが「検証との差」の入り口側
  exit    約定単価と日足の終値の差。15:20 の成行は引けではないので、滑りと時刻のずれが混じる
  exec    注文直前の時価（ref_*）と約定単価の差（入り + 出）。台帳の exec_bp の符号を反転したもの。
          **建玉を増やすと大きくなるのはここだけ**
  timing  入り: 注文直前の時価と始値の差 / 出: 終値と注文直前の時価の差。寄付から発注までと、
          15:20 から引けまでの値動きで、注文の大きさには依らない
  part    建値 × 株数 ÷ 寄付の約定代金（その日の最初に約定のあった 1 分足。特別気配で遅れた寄付もここ）

寄付の板寄せで約定した注文（約定単価 = 始値）は、自分の注文が始値そのものを動かすので、ここでは測れない
（exec は寄り前の気配との差になる）。ザラ場で約定した注文の exec を I0（今の規模での滑り）として読む。
2026-09-15〜17 の 12 件では、入りの差 +55bp のほとんどが timing で、ザラ場の exec は +3〜10bp だった。
"""
import argparse
import json
import subprocess
import sys

import duckdb
import numpy as np
import pandas as pd

MIN_TRADES = 60   # 20 営業日 × 3 銘柄。これに満たないうちは判断に使わない


def load(since):
    out = subprocess.run(
        ["bin/daytrade", "trades", "--config-dir", "config/daytrade_margin", "--from", since, "--json"],
        check=True, capture_output=True, text=True).stdout
    rows = json.loads(out)["rows"]
    t = pd.DataFrame(rows)
    if t.empty:
        return t
    t = t[t["priced"] == "actual"].copy()
    sign = np.where(t["side"] == "BUY", 1.0, -1.0)
    t["amount"] = t["entry"] * t["quantity"]
    t["entry_bp"] = sign * (t["entry"] / t["bar_open"] - 1) * 1e4
    t["exit_bp"] = sign * (1 - t["exit"] / t["bar_close"]) * 1e4
    t["exec"] = -t["exec_bp"]
    t["timing_in"] = sign * (t["ref_entry"] / t["bar_open"] - 1) * 1e4
    t["timing_out"] = sign * (1 - t["bar_close"] / t["ref_exit"]) * 1e4
    t["fill"] = np.where(t["entry"] == t["bar_open"], "板寄せ", "ザラ場")
    return t


def attach_open_value(t):
    days = sorted(t["day"].unique())
    files = [f"data/jquants/equities_bars_minute/{d}.parquet" for d in days]
    con = duckdb.connect()
    con.register("t", t[["day", "code"]].drop_duplicates())
    q = f"""
    SELECT Code AS code, CAST(Date AS VARCHAR) AS day,
           arg_min(Va, Time) FILTER (WHERE Va > 0) AS open_va,
           min(Time) FILTER (WHERE Va > 0) AS open_time
    FROM read_parquet({files!r}, union_by_name = true)
    WHERE (CAST(Date AS VARCHAR), Code) IN (SELECT day, code FROM t)
    GROUP BY 1, 2"""
    try:
        m = con.sql(q).df()
    except duckdb.IOException as e:   # 分足は 17:25 に取り込む。当日ぶんが無ければ前日までで測る
        print(f"分足が読めない日がある: {e}", file=sys.stderr)
        return t.assign(open_va=np.nan, open_time=None)
    return t.merge(m, on=["day", "code"], how="left")


def wmean(x, w):
    ok = x.notna() & w.notna()
    return (x[ok] * w[ok]).sum() / w[ok].sum() if ok.any() else float("nan")


def tstat(x):
    x = x.dropna()
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x))) if len(x) > 2 else float("nan")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--from", dest="since", default="2026-09-18")
    a = ap.parse_args()
    t = load(a.since)
    if t.empty:
        print(f"{a.since} 以降の本発注の約定が無い")
        return
    t = attach_open_value(t)
    t["part"] = t["amount"] / t["open_va"]
    print(f"{a.since}〜{t['day'].max()}  本発注 {len(t)} 件（{t['day'].nunique()} 日）"
          + ("" if len(t) >= MIN_TRADES else f"  ※ {MIN_TRADES} 件に満たない。判断に使わない"))
    for side, g in [("全体", t)] + list(t.groupby("side")):
        label = {"BUY": "ロング", "SELL": "ショート"}.get(side, side)
        print(f"\n[{label}] {len(g)} 件  1 注文 中央値 {g['amount'].median()/1e4:.0f} 万")
        for col, name in [("entry_bp", "入り（始値との差）"), ("exit_bp", "出（終値との差）"),
                          ("timing_in", "  うち寄付→発注の値動き"), ("timing_out", "  うち 15:20→引けの値動き"),
                          ("exec", "執行（入り + 出）")]:
            x = g[col]
            if x.notna().sum() == 0:
                continue
            print(f"  {name:24s} 金額加重 {wmean(x, g['amount']):+7.1f} bp  平均 {x.mean():+7.1f}（t {tstat(x):+5.2f}）"
                  f"  中央値 {x.median():+7.1f}  件数 {x.notna().sum()}")
        z = g[g["fill"] == "ザラ場"]
        print(f"  約定: 板寄せ {int((g['fill'] == '板寄せ').sum())} 件 / ザラ場 {len(z)} 件"
              f"  ザラ場の執行 金額加重 {wmean(z['exec'], z['amount']):+.1f} bp（{z['exec'].notna().sum()} 件）")
        p = g["part"].dropna()
        if len(p):
            print(f"  寄付の約定代金に占める割合  中央値 {100*p.median():.1f}%  上位 10% {100*p.quantile(.9):.1f}%  最大 {100*p.max():.1f}%"
                  f"  寄付が 9:00 でない {100*(g['open_time'].dropna() != '09:00').mean():.0f}%")
        ok = z["part"].notna() & z["exec"].notna()
        if ok.sum() >= 30 and z.loc[ok, "part"].quantile(.9) > 4 * z.loc[ok, "part"].quantile(.1):
            s_ = np.sqrt(z.loc[ok, "part"])
            c = (s_ * z.loc[ok, "exec"]).sum() / (s_ * s_).sum()
            r = np.corrcoef(s_, z.loc[ok, "exec"])[0, 1]
            print(f"  ザラ場の執行 = c × √割合 の c = {c:+.1f} bp（相関 {r:+.2f}。割合 5% で {c*np.sqrt(.05):+.1f} bp）")

    z = t[t["fill"] == "ザラ場"]
    print(f"\n検証との差（入り + 出、金額加重）: {wmean(t['entry_bp'], t['amount']) + wmean(t['exit_bp'], t['amount']):+.1f} bp"
          f"  うち執行（ザラ場、= I0 の目安）: {wmean(z['exec'], z['amount']):+.1f} bp")
    print("試算の前提との比べ（2026-09-jp-daytrade-margin-80pct-compound、上限 1500 万の年平均）:"
          " I0 = 5bp → 最終資産 8,655 万 / 10bp → 8,140 万 / 20bp → 7,109 万。backtest の滑りは一律 5bp")


if __name__ == "__main__":
    main()
