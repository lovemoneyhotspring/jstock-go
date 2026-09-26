#!/usr/bin/env bash
# 毎年 8 月: JPX日経中小型株指数の定期入替の発表を見つけ、採用銘柄の注文一覧を vault に書いて Discord で知らせる。
# 発注は人が別の証券会社で行う（test/jsm_announce.py の docstring）。このスクリプトは発注しない。
# cron: 30 17,19,21 1-20 8 *（発表は 8 月上旬の引け後。日と曜日を両方指定すると OR になるので曜日は付けない。
# 見つけた後の回は「もうある」で何もしない）
# ログは state/logs/jsm-announce.log。
set -u
cd "$(dirname "$0")/.."
HOME_DIR="$(pwd)"
VAULT_DIR="${VAULT_DIR:-$HOME/obsidian-vault}"
YEAR="$(date +%Y)"
NOTE="20-research/${YEAR}-08-jsm-adds.md"
POST_BIN="${POST_BIN:-$HOME_DIR/bin/discord-post}"
ERR=state/logs/jsm-announce.err

# Discord の送り先は .env にしかない（deploy/report.sh の with_dotenv と同じく、送るときだけ読む）
post() {
  [ -x "$POST_BIN" ] || { echo "discord-post が無い"; return 0; }
  ( if [ -f "$HOME_DIR/.env" ]; then set -a; . "$HOME_DIR/.env"; set +a; fi
    printf '%s\n' "$2" | timeout -k 10 120 "$POST_BIN" --title "$1" ) || echo "Discord に送れなかった: $1"
}

echo "== $(date '+%F %T')"
[ -f "$VAULT_DIR/$NOTE" ] && { echo "もうある: $NOTE"; exit 0; }
OUT="$(test/.venv/bin/python test/jsm_announce.py --vault "$VAULT_DIR" 2> >(grep -v -E 'UserWarning|m = df\.title|fontTools' >"$ERR"))"
RC=$?
echo "$OUT"
case $RC in
  0) ;;
  3) # まだ発表が無い。例年は 8/5〜8/7 なので、8/12 以降も見つからなければ JPX のページの形が変わった疑い。
     # 1 日 1 回（19 時台の回）だけ知らせる
     if [ "$(date +%-d)" -ge 12 ] && [ "$(date +%H)" = "19" ]; then
       post "中小型株指数の採用: 発表が見つからない" "8/12 を過ぎても ${YEAR} 年の定期入替の発表が見つからない。JPX のページ（www.jpx.co.jp/markets/indices/jpx-nikkei400/）を人が確かめ、
見つけたら test/.venv/bin/python test/jsm_announce.py --url <発表ページ> で一覧を作る"
     fi
     exit 0 ;;
  2) post "中小型株指数の採用: 人が確かめる" "発表は見つかったが、PDF の件数が本文と合わないか PDF が無い。${OUT}
手で読むなら test/jsm_announce.py --url <発表ページ> --dry-run"; exit 1 ;;
  *) post "中小型株指数の採用: 失敗" "test/jsm_announce.py が失敗（終了コード $RC）。$(tail -5 "$ERR")"; exit 1 ;;
esac
[ -f "$VAULT_DIR/$NOTE" ] || exit 0
# commit はこのノートだけ。先に commit してから取り込む（deploy/report.sh と同じ）
git -C "$VAULT_DIR" add "$NOTE"
if git -C "$VAULT_DIR" commit --quiet -m "research: ${YEAR} 年 8 月の中小型株指数の採用の注文一覧" -- "$NOTE"; then
  git -C "$VAULT_DIR" pull --rebase --autostash --quiet && git -C "$VAULT_DIR" push --quiet origin main \
    || echo "vault の push に失敗（commit は済んでいる）"
fi
post "中小型株指数の採用（明日の寄成）" "$(echo "$OUT" | tail -1)"
