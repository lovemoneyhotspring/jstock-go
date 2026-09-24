# 通知とログの共通部品（deploy/guard-preopen.sh・deploy/close-net.sh から source する）。
# cron とは別系統（systemd のタイマー）から呼ばれるので、環境を自分で整える。

HOME_DIR="${WBJP_HOME:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$HOME_DIR"
export WBJP_ENV="${WBJP_ENV:-prod}"
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"
GUARD_LOG="${GUARD_LOG:-$HOME_DIR/state/logs/systemd-guard.log}"
mkdir -p "$(dirname "$GUARD_LOG")"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$GUARD_LOG"; }

# notify <題> <本文>: Discord に 1 通。送れなくてもログに残して先へ進む
notify() {
  log "[notify] $1 :: $2"
  [ -x "$POST_BIN" ] || return 0
  printf '%s\n' "$2" | timeout -k 10 60 "$POST_BIN" --title "$1" >> "$GUARD_LOG" 2>&1 || log "[warn] 通知を送れませんでした: $1"
}
