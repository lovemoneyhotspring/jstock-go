"""寄る前の open の回（寄成）が、9:00:00 の板寄せまでにどれだけ余裕を残して終わったかを朝ごとに見る。

DT_PREOPEN_AT（deploy/crontab.txt）を刻んで遅らせている途中の点検用（2026-09-21〜）。遅いほど気配は始値に近いが、
最後の注文が 9:00:00 に間に合わなければ寄成は板寄せに乗らない（遅れた寄成の行き先は実機で未検証。docs/BROKER_VERIFY.md）。

  test/.venv/bin/python test/dt_preopen_timing.py [--since 2026-09-24]

- started / ended: 回の開始と終了（open_run の記録時刻 − 所要）
- first_sent / last_acked: 新規注文（CLMKabuNewOrder）の 1 本目を送った時刻と、最後の 1 本の応答を受けた時刻。
  構造化ログ（state/logs/daytrade-prod.jsonl*）の broker.request から取る——履歴（execution の intent）は
  回の終わりにまとめて書くので、注文ごとの時刻にならない
- margin_s: last_acked から 9:00:00 までの秒。**これが 2 秒を切る朝が出たら、それ以上は遅らせない**
- rejected: 応答の result_code が 0 でない注文の数
- fills: その日の約定の数（寄成が通ったかは fill_price が始値と一致するかで見る）
"""

import argparse

import duckdb
import pandas as pd

HIST = "state/daytrade/history"
LOGS = "state/logs/daytrade-prod.jsonl*"

SQL = f"""
WITH r AS (
  SELECT CAST(day AS DATE) d, run_id, mode, outcome, orders, elapsed_ms,
         timezone('Asia/Tokyo', recorded_at) ended, timezone('Asia/Tokyo', recorded_at) - to_milliseconds(elapsed_ms) started
  FROM read_parquet('{HIST}/open_run/*.parquet', union_by_name=true) WHERE CAST(day AS DATE) >= CAST(? AS DATE)),
p AS (SELECT * FROM r WHERE hour(started) < 9),
l AS (
  SELECT run_id, timezone('Asia/Tokyo', CAST(ts_utc AS TIMESTAMPTZ)) acked,
         timezone('Asia/Tokyo', CAST(ts_utc AS TIMESTAMPTZ)) - to_milliseconds(CAST(json_extract_string(extra, '$.elapsed_ms') AS BIGINT)) sent,
         json_extract_string(extra, '$.result_code') rc, json_extract_string(extra, '$.p_errno') errno
  FROM read_json('{LOGS}', format = 'newline_delimited', ignore_errors = true,
                 columns = {{ts_utc: 'VARCHAR', run_id: 'VARCHAR', code: 'VARCHAR', extra: 'JSON'}})
  WHERE code = 'broker.request' AND json_extract_string(extra, '$.clm') = 'CLMKabuNewOrder'),
i AS (SELECT run_id, count(*) n, min(sent) first_at, max(acked) last_at,
             sum((coalesce(rc, '0') <> '0' OR coalesce(errno, '0') <> '0')::INT) rejected FROM l GROUP BY 1),
f AS (SELECT CAST(day AS DATE) d, count(*) fills FROM read_parquet('{HIST}/execution/*.parquet', union_by_name=true) WHERE event = 'fill' GROUP BY 1)
SELECT strftime(p.d, '%m-%d') AS day, p.mode, p.outcome, strftime(p.started, '%H:%M:%S.%g') AS started, strftime(p.ended, '%H:%M:%S.%g') AS ended,
       p.elapsed_ms, coalesce(i.n, 0) AS orders, strftime(i.first_at, '%H:%M:%S.%g') AS first_sent, strftime(i.last_at, '%H:%M:%S.%g') AS last_acked,
       round(epoch(date_trunc('day', p.started) + INTERVAL 9 HOUR) - epoch(i.last_at), 2) AS margin_s, coalesce(i.rejected, 0) AS rejected, coalesce(f.fills, 0) AS fills
FROM p LEFT JOIN i USING (run_id) LEFT JOIN f USING (d) ORDER BY p.started
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-24")
    a = ap.parse_args()
    pd.set_option("display.width", 220)
    r = duckdb.sql(SQL, params=[a.since]).df()
    print(r.to_string(index=False) if not r.empty else f"{a.since} 以降に 9:00 より前の open の回がありません")


if __name__ == "__main__":
    main()
