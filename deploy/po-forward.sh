#!/usr/bin/env bash
# 公募増資の P1 の前向きの記録（test/po_forward.py）を回し、vault のノートだけを commit・push する。
# 紙の上の記録で、発注はしない。cron: 45 19 * * 1-5（J-Quants の日足と wbjp data sync の後）
# 失敗しても本番の売買には関係しない。ログは state/logs/po-forward.log。
set -u
cd "$(dirname "$0")/.."
VAULT_DIR="${VAULT_DIR:-$HOME/obsidian-vault}"
NOTE="20-research/2026-09-jp-public-offering-forward.md"
ERR=state/logs/po-forward.err

echo "== $(date '+%F %T')"
if ! test/.venv/bin/python test/po_forward.py --vault "$VAULT_DIR" 2> >(grep -v -E 'UserWarning|m = df\.title' >"$ERR"); then
  echo "po_forward.py が失敗（$ERR）"
  tail -5 "$ERR"
  exit 1
fi
[ -d "$VAULT_DIR/.git" ] || exit 0
# commit はこのノートだけ（人が vault で add しかけていた変更を巻き込まない）。先に commit してから取り込む
git -C "$VAULT_DIR" add "$NOTE"
if git -C "$VAULT_DIR" diff --cached --quiet -- "$NOTE"; then
  echo "vault: 変更なし"
  exit 0
fi
if git -C "$VAULT_DIR" commit --quiet -m "research: 公募増資の前向きの記録 $(date +%F)" -- "$NOTE"; then
  git -C "$VAULT_DIR" pull --rebase --autostash --quiet \
    && git -C "$VAULT_DIR" push --quiet origin main \
    || echo "vault の push に失敗（commit は済んでいる。次回の実行で一緒に上がる）"
else
  echo "vault への commit に失敗（ノートは $VAULT_DIR/$NOTE に残っている）"
fi
