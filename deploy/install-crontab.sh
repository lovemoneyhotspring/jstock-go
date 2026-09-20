#!/usr/bin/env bash
# jstock-go の cron ブロックだけを差し替える（暗号資産の他ジョブ・rcguard はそのまま）。
#
#   deploy/install-crontab.sh            # バックアップを取って差し替える
#   deploy/install-crontab.sh --dry-run  # 差し替えたあとの crontab との差分だけ見る
#
# docs/DEPLOY.md の手順（行番号を手で調べて sed で挟む）を、目印の行で自動にしたもの。
#   ブロックの先頭 … `# wbjp（` で始まる行（deploy/crontab.txt の 1 行目）
#   ブロックの末尾 … `# >>> rcguard` の直前
# 目印が見つからない・順序が逆なら何もしない。差し替えたら state/crontab.good に全体を控える
# （deploy/guard-preopen.sh が、crontab が消えたときにここから戻す）。
set -euo pipefail

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
CRONTAB="${CRONTAB:-crontab}"
cd "$HOME_DIR"

dry=0
[ "${1:-}" = "--dry-run" ] && dry=1

die() { echo "install-crontab: $*" >&2; exit 1; }

mkdir -p state/backup/crontab
bk="state/backup/crontab/crontab-$(date +%Y%m%d-%H%M%S).txt"
"$CRONTAB" -l > "$bk" 2>/dev/null || : > "$bk"

start=$(grep -n '^# wbjp（' "$bk" | head -1 | cut -d: -f1 || true)
end=$(grep -n '^# >>> rcguard' "$bk" | head -1 | cut -d: -f1 || true)
[ -n "$start" ] || die "$bk に '# wbjp（' で始まる行が無い（ブロックの先頭を特定できない）"
[ -n "$end" ] || die "$bk に '# >>> rcguard' が無い（ブロックの末尾を特定できない）"
[ "$start" -lt "$end" ] || die "目印の順序が逆（先頭 $start 行 / 末尾 $end 行）"
head -1 deploy/crontab.txt | grep -q '^# wbjp（' || die "deploy/crontab.txt の 1 行目が '# wbjp（' で始まらない"

new="$(mktemp)"
trap 'rm -f "$new"' EXIT
{ sed -n "1,$((start - 1))p" "$bk"; cat deploy/crontab.txt; echo; sed -n "${end},\$p" "$bk"; } > "$new"

# ブロックの外（他のジョブ・rcguard）が 1 行も変わっていないこと
outside() { sed -n "1,$((start - 1))p" "$1"; sed -n "${2},\$p" "$1"; }
new_end=$(grep -n '^# >>> rcguard' "$new" | head -1 | cut -d: -f1)
cmp -s <(outside "$bk" "$end") <(outside "$new" "$new_end") || die "ブロックの外が変わってしまう（中止）"

"$CRONTAB" -n "$new" >/dev/null || die "新しい crontab の構文が通らない"

if [ "$dry" -eq 1 ]; then
  diff <("$CRONTAB" -l) "$new" || true
  echo "install-crontab: --dry-run（差し替えません）。バックアップ: $bk"
  exit 0
fi
"$CRONTAB" "$new"
cp "$new" state/crontab.good
echo "install-crontab: 差し替えました。バックアップ: $bk / 控え: state/crontab.good"
echo "  daytrade の行: $(grep -v '^[[:space:]]*#' "$new" | grep -c 'WBJP_BIN/daytrade' || true)"
