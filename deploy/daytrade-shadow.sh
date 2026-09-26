#!/usr/bin/env bash
# daytrade の影の記録（並べ方 LBZ2 / gap_vol × 配分 F6 / R7 の 4 通りで、その日に建てたらいくらだったか）を作り、
# vault（99-attachments/daytrade-shadow/）に commit・push する。平日 20:40、evaluate（20:20）の後に cron から。
# 発注しない・立花に繋がない。材料は state/daytrade/history と data/jquants（test/dt_live_shadow.py の先頭）。
#
# 2026-09-28 から LBZ2 と F6 を同時に入れたので、どちらが効いたかは採らなかった方の記録と比べて後から切り分ける。
# あわせて state/daytrade/history を state/backup/daytrade-history へ複製する（accum backup は SQLite だけで、
# history の parquet は対象外だった）。
set -uo pipefail
HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
VAULT_DIR="${VAULT_DIR:-$HOME/obsidian-vault}"
OUT="$VAULT_DIR/99-attachments/daytrade-shadow"
cd "$HOME_DIR" || exit 1

mkdir -p state/backup/daytrade-history
rsync -a state/daytrade/history/ state/backup/daytrade-history/ || echo "history の複製に失敗" >&2

PYTHONPATH=test test/.venv/bin/python test/dt_live_shadow.py --out "$OUT" || { echo "影の記録の作成に失敗" >&2; exit 1; }

# commit はこの置き場だけ（人が vault で書きかけのノートを巻き込まない。report.sh と同じ作法）
git -C "$VAULT_DIR" add "$OUT" || exit 1
if git -C "$VAULT_DIR" diff --cached --quiet -- "$OUT"; then
  echo "vault: 影の記録に変更なし" >&2
  exit 0
fi
git -C "$VAULT_DIR" commit --quiet -m "data(daytrade-shadow): $(date +%F)" -- "$OUT" \
  && git -C "$VAULT_DIR" pull --rebase --autostash --quiet \
  && git -C "$VAULT_DIR" push --quiet origin main
