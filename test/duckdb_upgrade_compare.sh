#!/usr/bin/env bash
# go-duckdb を上げる前後で backtest の結果とメモリのピークを突き合わせる。
#
#   test/duckdb_upgrade_compare.sh <bin のディレクトリ> <ラベル>
#
# 例（旧版を作業領域にビルドしてから）:
#   go build -o /path/old/ ./cmd/daytrade ./cmd/wbjp ./cmd/accum ./cmd/jquants
#   test/duckdb_upgrade_compare.sh /path/old old
#   go build -o /path/new/ ...（本番の bin/ には入れない）
#   test/duckdb_upgrade_compare.sh /path/new new
#   diff test/out/duckdb-upgrade/old test/out/duckdb-upgrade/new
#
# 重い検証は 1 本ずつ（並列にするとメモリ不足で止まる）。期間は固定して、
# 20:00 の取り込みで「最新」がずれても前後が同じデータを読むようにする。
# daytrade は --no-cache でパネルを毎回 DuckDB から組み直す（キャッシュを読むと比較にならない）。
# 旧版（DuckDB 1.1.3）の daytrade は既定の 3GB で Out of Memory になる。WBJP_DUCKDB_MEMORY_LIMIT=5GB を付けて回す。
set -euo pipefail

bindir=${1:?bin のディレクトリ}
label=${2:?ラベル}
until=${UNTIL:-2026-09-24}
out=test/out/duckdb-upgrade/$label
mkdir -p "$out"

run() {
	local name=$1
	shift
	echo "== $name: $*"
	/usr/bin/time -v -o "$out/$name.time" "$@" >"$out/$name.out" 2>"$out/$name.err" || {
		echo "   失敗（終了 $?）: $out/$name.err"
		return 0
	}
	grep -E "Maximum resident|Elapsed" "$out/$name.time" | sed 's/^\s*/   /'
}

run daytrade "$bindir/daytrade" backtest --no-cache --until "$until"
run daytrade-minute "$bindir/daytrade" backtest --no-cache --until "$until" --fill-entry 09:01 --fill-exit 15:20
run wbjp "$bindir/wbjp" backtest --to "$until"
run accum "$bindir/accum" backtest --to "$until"
run jquants-query "$bindir/jquants" query "select version() as v, count(*) as n from range(1000000)"
