# その朝の open を回（run_id）ごとに要約する。deploy/morning-check.sh が使う。
#
#   jq -sr --arg d 2026-09-15 -f deploy/open-pipeline.jq state/logs/daytrade-prod.jsonl
#
# 1 回につき: 時刻と結末（見送りの理由 / 発注の件数）、気配の内訳、前夜の米国の値、
# 材料（TOB・MBO など）でショートから外した銘柄と、記録簿が古くてショートを見送った理由。
# 発注した回は、脚ごとに N・1 注文の予算と、最後に選ばれた順位までの全行
# （選んだ銘柄の株数、外れた銘柄の理由 = selection.PickReasons）、出した注文。
#
# 気配の数字:
#   除外       鮮度の検査で落とした（古い = 現在値時刻が古く板も無い / 遅延 = 遅延気配）
#   板で残す   現在値時刻は古いが板が返っていたので残した（約定の薄い銘柄。除外ではない）
#   未来       時刻が未来の気配。数えるだけで除外しない（前日 15:30 が返る穴の見張り）
#   板から値段 まだ寄っていないので値段を最良気配から取った（寄りが遅れる銘柄）
#   使えた     順位付けに渡した件数（2026-09-15 より前のログには無い）

def commas: tostring | if length > 3 then (.[:-3] | commas) + "," + .[-3:] else . end;
def yen: tonumber | floor | commas;
def jst: (.ts_utc[11:13] | tonumber + 9 | tostring) + ":" + .ts_utc[14:16];
def reason_ja:
  if . == null then "理由の記録なし"
  else ({
    "picked": "選定",
    "over_budget": "1 単元が予算超え",
    "sector_cap": "業種の上限",
    "value_pool": "益回りで N に入らず",
    "too_small": "按分が 1 単元未満",
    "beyond_n": "N の外"
  }[.] // .)
  end;

# $d は **JST の日付**。8:59:45（寄る前）の回は UTC では前日の 23:59 なので、
# UTC の日付で絞ると漏れる。ts_utc を JST に直してから日付を比べる
def jst_day: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 + 32400 | strftime("%Y-%m-%d");

[ .[] | select(.command == "open" and ((.ts_utc | jst_day) == $d)) ]
| group_by(.run_id)
| sort_by(.[0].ts_utc)
| .[]
| . as $run
| (map(select(.code == "daytrade.quotes" and .extra.requested != null)) | first | .extra) as $got
| (map(select(.code == "daytrade.quotes" and .extra.book_kept != null)) | first | .extra) as $fresh
| (map(select(.code == "daytrade.us_session")) | first | .extra.session) as $us
| (map(select(.code == "daytrade.skip")) | first) as $skip
| (map(select(.code == "daytrade.run")) | first | .extra) as $done
| ( if $skip then
      "見送り: \($skip.msg)" + (if ($skip.extra.reasons | type) == "array" then "（\($skip.extra.reasons | join("、"))）" else "" end)
    elif $done then "発注 \($done.picks) 件（失敗 \($done.failures)）"
    else "結末のログなし（途中で止まった・締め切り）" end ) as $outcome
| "\($run[0] | jst)  \($outcome)",
  ( if $got then
      "  気配 受信 \($got.received)/\($got.requested)"
      + "・除外 \(($fresh.stale // 0) + ($fresh.delayed // 0))（古い \($fresh.stale // 0) / 遅延 \($fresh.delayed // 0)）"
      + "・板で残す \($fresh.book_kept // 0)・未来 \($fresh.future // 0)・板から値段 \($got.from_book // 0)"
      + (if $fresh.usable != null then "・使えた \($fresh.usable)" else "" end)
    else empty end ),
  ( if $us then "  米国 \($us)" else empty end ),
  ( $run[] | select(.code == "daytrade.news_stale") | "  ショートを見送り（ニュースの記録簿）: \(.extra.reason)" ),
  ( $run[] | select(.code == "daytrade.corp_event") | .extra
    | "  材料でショートから外した: \(.symbol) \(.name // "")（\(.kind)）\(.at) \(.headline)" ),
  ( $run[] | select(.code == "daytrade.ranking") | .extra as $r
    # 誰も選ばれなかった回も出す。「上位が外れた理由」を追う機能は、まさにその回で一番効く
    # （9:04 に picked 0 で 5 行あるのに何も出ない、が起きていた）
    | (([ $r.rows[] | select(.picked) | .rank ] | max) // (($r.n // 0) + 5)) as $last
    | if ($r.rows | length) == 0 then empty else
        "  \($r.side) N=\($r.n) 1 注文 \($r.budget | yen) 円（気配 \($r.quotes) 銘柄から）",
        ( $r.rows[] | select(.rank <= $last)
          | "    #\(.rank) \(.symbol) \(.name // "")  ギャップ \(.gap | tonumber * 10000 | round / 100)%  \(.price | yen) 円  "
            + ( if .picked then "→ \(.quantity) 株"
                else "外れた: " + ((.reason // (if (.price | tonumber) * 100 > ($r.budget | tonumber) then "over_budget" else null end)) | reason_ja)
                end ) )
      end ),
  ( $run[] | select(.code == "daytrade.order") | .extra
    # 持ち越しの返済は price・amount・side を持たない（execute/carry.go）。同じ形と決めつけて
    # yen（tonumber）に渡すと jq がその場で落ち、点検の本文が途中で切れる
    | if .amount == null then
        "  返済 \(.symbol) \(.quantity) 株（\(.outcome)）"
      else
        "  注文 \(.side) \(.symbol) \(.quantity) 株 @\(.price | yen) = \(.amount | yen) 円（\(.outcome)）"
      end )
