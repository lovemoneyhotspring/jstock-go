"""寄指（寄付条件つきの指値）を見える値段に置いたら何本約定するか: 見えるギャップの帯ごとの約定率。

根拠: vault 20-research/2026-09-jp-daytrade-preopen-order.md（「寄指の約定率」の追記、2026-09-21）

  test/.venv/bin/python test/dt_limit_fill_rate.py [--slot 0859] [--since 2026-09-11] [--top 10]

見える値段は寄る前の最良気配の中値（test/dt_preopen_rank.py と同じ）。指値 L = 見える値段 × (1 + m)。
約定は「始値 < L」（m = 0）か「始値 <= L」（m > 0）。始値 = L は板寄せで約定が保証されないので m = 0 では数えない。

**見えるギャップで条件を付ける**のが要点。始値のギャップの帯で見た誤差（偏りほぼ 0）とは向きが逆になる——
深く見える銘柄ほど「深く見えすぎている」ものが集まる（選んだ側の偏り）。本番は見える側でしか選べない。

損益は出さない（採否は事前登録した模擬 test/dt_preopen_sim.py --limit-on-open で決める）。
"""
import argparse

import duckdb

BOOK = "state/daytrade/history/book/*.parquet"
BARS = "data/jquants/equities_bars_daily/*.parquet"
MIN_TURNOVER = 1e8
MARGINS = (0.0, 0.005, 0.01, 0.02)

BASE = f"""
WITH b AS (
  SELECT CAST(day AS DATE) d, symbol, TRY_CAST(pPRP AS DOUBLE) pc,
         TRY_CAST(pQAP AS DOUBLE) ask, TRY_CAST(pQBP AS DOUBLE) bid
  FROM read_parquet('{BOOK}', union_by_name=true)
  WHERE slot = ? AND CAST(day AS DATE) >= CAST(? AS DATE)),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op, TRY_CAST(Va AS DOUBLE) va
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE)),
n AS (
  SELECT b.d, (ask + bid) / 2 vis, op, pc, ((ask + bid) / 2 / pc - 1) * 100 gv,
         rank() OVER (PARTITION BY b.d ORDER BY (ask + bid) / 2 / pc) rk
  FROM b JOIN q ON q.d = b.d AND q.code = b.symbol || '0'
  WHERE ask > 0 AND bid > 0 AND pc > 0 AND op > 0 AND va >= {MIN_TURNOVER} AND (ask + bid) / 2 < pc)
"""


def fill(m):
    return f"(op {'<' if m == 0 else '<='} vis * {1 + m})"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--slot", default="0859")
    ap.add_argument("--since", default="2026-09-11")
    ap.add_argument("--top", type=int, default=10, help="日ごとに、見えるギャップが深い順の上位何本で約定本数を数えるか")
    a = ap.parse_args()
    args = [a.slot, a.since, a.since]
    rates = ", ".join(f"round(avg({fill(m)}::INT) * 100, 1) \"m={m * 100:g}%\"" for m in MARGINS)
    counts = ", ".join(f"sum({fill(m)}::INT) \"m={m * 100:g}%\"" for m in MARGINS)

    print(f"slot {a.slot}、{a.since}〜、売買代金 {MIN_TURNOVER / 1e8:g} 億以上、見えるギャップ < 0\n")
    print("見えるギャップの帯ごとの約定率（%）。err は 始値 ÷ 見える値段 − 1 の中央値（%pt）")
    print(duckdb.execute(BASE + f"""
        SELECT CASE WHEN gv < -5 THEN '1: -5% 以下' WHEN gv < -3 THEN '2: -5〜-3%'
                    WHEN gv < -1 THEN '3: -3〜-1%' ELSE '4: -1〜0%' END band,
               count(*) n, round(median((op / vis - 1) * 100), 2) err, {rates}
        FROM n GROUP BY 1 ORDER BY 1""", args).df().to_string(index=False))

    print("\n日ごと（全体）。約定は日どうしで固まる——模擬の誤差は日をまたいで独立に引いてはいけない")
    print(duckdb.execute(BASE + f"SELECT d, count(*) n, {rates} FROM n GROUP BY d ORDER BY d", args).df().to_string(index=False))

    print(f"\n日ごと、見えるギャップが深い順の上位 {a.top} 本の約定本数（seen / true はギャップの平均 %）")
    print(duckdb.execute(BASE + f"""
        SELECT d, round(avg(gv), 2) seen, round(avg((op / pc - 1) * 100), 2) "true", {counts}
        FROM n WHERE rk <= {a.top} GROUP BY d ORDER BY d""", args).df().to_string(index=False))


if __name__ == "__main__":
    main()
