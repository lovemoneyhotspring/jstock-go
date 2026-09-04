# デイトレ（`daytrade`）— 寄付で買い、大引で売る

日本株のギャップ逆張り `jp_gap_fade` を回す CLI。根拠と数字は
[research/2026-08-jp-daytrade-selection.md](research/2026-08-jp-daytrade-selection.md)。
設定は `config/daytrade/daytrade.toml`、実装は `pkg/daytrade/`（発注は `pkg/daytrade/execute`）。

## 規則（1 行で）

前夜に「プライム・売買代金 20 日中央値 1 億円以上・時価総額下位 1/3 除外・前日引け後の決算なし・
当日決算予定なし・日々公表信用残の対象でない」銘柄を母集団にし、9:00 の気配で
**ギャップ（寄付 ÷ 前日終値 − 1）が最も小さい N 銘柄**を成行で買い、15:20 以降の成行売りで
手仕舞う（15:25 以降ならクロージング・オークションで引け値）。持ち越さない。

N は資金から決める: `N = round(max_capital ÷ order_budget)`（200 万円 ÷ 67 万円 → 3）。
1 注文は `max_capital ÷ N`。手数料は立花証券の定額コース（`daytrade.fees`。現物は 1 日の約定代金
合計で段階、信用は 0 円）。現物の段階は合計で決まるので、1 注文の大きさより「その日の合計が
段階の境目を越えるか」が効く。`order_budget` の既定 67 万円は旧来の一律手数料を前提に研究した値で、
定額コースでの最適値は再検証していない。

## 1 日の流れ（JST）

| 時刻 | コマンド | 何をするか | 入力 |
|---|---|---|---|
| 20:30（前夜） | `daytrade plan` | 翌営業日の母集団を `state/daytrade/plan-<日付>.parquet` に保存 | J-Quants アーカイブ（`jquants sync` 済みの前日足・銘柄一覧・決算・日々公表） |
| 09:00〜09:15 | `daytrade open --live --yes` | 候補の気配を取り、ギャップ下位 N 銘柄を成行買い。台帳に記録 | plan + 気配（`execution.quote_source`） |
| 15:20〜15:30 | `daytrade close --live --yes` | 台帳の当日買いをブローカーに照会し、約定数量を成行売り（15:20 はその場で約定。15:25 以降はクロージング・オークションで引け値） | 台帳 + ブローカー |
| 15:40 | `daytrade verify` | 今日の売りが全部約定したか照会し、売れ残り（持ち越し）を通知 | 台帳 + ブローカー |
| 随時 | `daytrade status` | 候補と当日の注文 | |

すべて既定は dry-run。`--live` が無ければ判断と記録だけで注文は出さない。本番口座では
`WBJP_ENV=prod` と `--live`、cron では `--yes` も要る（`accum` と同じ規律）。

- `open` は台帳に当日の買い（dry-run 以外で、生きている／約定したもの）があれば何もしない（冪等）。注文 ID は日付・銘柄・数量から決まる。買いが拒否されていれば次の回（9:04 / 9:07）で種を変えて送り直す
- `close` は台帳の買いだけを売る。**ブローカーの建玉を無条件に売らない**（積立の保有を守る）
- `close` の失敗は持ち越しになるので必ず通知する（`WBJP_ALERT_WEBHOOK_URL`）
- IV ゲート（`regime.iv_gate`）は前日の日経 225 オプション `BaseVol` 中央値。オプションの足は 27:00 頃の更新なので
  20:30 の plan には無く、`open` が朝のアーカイブから取り直す。無ければゲート無しで進む（警告ログ）

## 配分と縮小（MaxDD 対策）

最大 DD は急落ではなく「効かない期間が続く」型（2025-10〜2026-02 の 4.5 か月で −50 万）。対策は 2 つで既定で有効:

- **ボラの逆数で配分**（`capital.weighting = "inverse_vol"`）: 上位 N の中で、20 日の日次ボラが大きい銘柄を少なく、
  穏やかな銘柄を多く持つ。利益 +8%・Sharpe 2.02 で DD は同じ
- **資産曲線で縮小**（`regime.equity_curve_days = 20` / `equity_curve_scale = 0.5`）: 直近 20 日の実現損益（台帳）が 0 以下なら
  その日の資金を半分にする。縮めている日は全体の約 2 割

資金が増えて DD の絶対額を許容できるようになったら、「順位順に 1 銘柄の上限まで詰め込み、余りを次点へ」（資金稼働 83%→96%、
利益 +9%、DD +12%）を再検討する（研究ノート「配分規則の比較」）。

## ストップ高・ストップ安

- **寄りでストップ高**: ギャップが正なので買い対象にならない。保有中にストップ高になれば買い殺到の板なので売るのは容易
- **寄りでストップ安**: ギャップダウンの候補に入るが、**買わない**（`signal.skip_limit_down`、既定 true）。
  売り殺到の板では買いは即約定するが、引けの売りは板に買いが無く**約定しない＝持ち越し**になる。研究では
  ストップ安に触れた日の取引は 78 件（1%）で勝率 9%・平均 −14,156 円、うち 53 件は引けもストップ安だった
- **保有中にストップ安**: 15:20 以降の売りが約定しないことがある。引け後の `daytrade verify`（15:40）が
  売りの約定をブローカーに照会し、売れ残りがあれば通知する（`daytrade.carry`）。翌朝に手で売る。
  バックテストはショートと同じく `margin.carry_penalty` で「翌寄りで売った」ことにして織り込む（研究では 0.5%）

## 信用売りの脚 `jp_gap_up_short`（`config/daytrade_margin`）

`config/daytrade_margin/daytrade.toml` は `extends = "../daytrade"` でロング側の規則を `config/daytrade` から
継ぎ、資金（`[capital]`）と `[margin]` だけを書く。ロング側を直すときは `config/daytrade` を直せば両方に効く。
子に同じ項目を書けばそちらが優先される（配列は丸ごと置き換え）。

立花証券 e支店（信用手数料 0 円）で、ロング `jp_gap_fade` に**信用売り**の脚を足す。根拠と数字は
[research/2026-09-jp-gap-up-short.md](research/2026-09-jp-gap-up-short.md)。

**規則（1 行で）**: 前夜に「貸借銘柄（`Mrgn == 2`）× プライム × 売買代金 20 日中央値 1 億円以上 × 前日引け後の決算なし ×
当日決算予定なし × **売り禁でない**」を母集団（plan の `short_eligible`）にし、9:00 の気配で
**ギャップ（寄付 ÷ 前日終値 − 1）が +5% 以上の銘柄を大きい順に N**（既定 3、1 注文 67 万円）成行で新規売り、
15:20 以降の成行返済買いで手仕舞う。常時建てる（ロングの資産曲線の合図には連動しない）。

- 母集団はロングの `[universe]` と**別**（`[margin]` の `segments` / `min_turnover` / `exclude_*`）。ショートは小型に効きが厚いので
  時価総額の分位で外さず、日々公表・注意喚起・増担保も外さない（建てられる規制銘柄に効きの差は無い）。
  **売り禁**（日証金の申込停止。`markets/margin-alert` の `PubReason.RestrictedByJSF`）だけ外す——新規売りが出せないため
- 決算翌日のギャップアップは順張り（−30 bp/取引）なので必ず外す。プライム限定なのは**張り付き**（下記）のため
- 検証（2017-01〜2026-08、現金 200 万・ロング 300 万 N3 + ショート 200 万 N3・滑り 5 bp・12 月休みと前夜の米国ゲート込み）:
  合算 1,923 万 / Sharpe 2.35 / MaxDD −82 万、うちショート 711 万 / 1.12 / −102 万（張り付きを全額計上）。負け年 0

**最大のリスクは張り付き**: 引けがストップ高（買い気配に張り付き）だと返済買いが約定せず持ち越しになる。研究では
全区分で 5.4%（プライム 2.1%）、翌朝までにさらに平均 −425 bp。場中の損切りは逆効果（往復ビンタ）で、
対策はプライム限定と、`verify`（15:40）の持ち越し通知 → 翌朝に手で返済。バックテストは `margin.carry_penalty`
（既定 1.0 = 張り付きは全て翌寄りで返済したとみなす）でこれを織り込む。実際の約定率は運用で測り、係数を決める。

ロング側は `jp_gap_fade` の DD 対策（合図の日に 1/2 に縮小）を**外す**（`margin.long_shrink = false`）。
信用買いなので建玉は現金より大きくでき、既定はロング 300 万円（現金 200 万・保証金率 33%）。経緯は
研究ノート「信用売りでの下げ相場対策」「ロングの DD 抑制を外す」（2026-08-jp-daytrade-selection.md）。
`daytrade backtest --config-dir config/daytrade_margin`。

**発注側**: `[margin]` が有効なら `plan` は `short_eligible` / `jsf_stop` 列を持ち、`open` はロングを信用買い（`long_via_margin`）、
ショートを `short_eligible` の信用新規売りで出し（気配はロング・ショートの和集合で取る）、`close` は反対売買の返済
（買建→返済売り、売建→返済買い）、`verify` は脚（long / short）ごとに約定と建玉を突き合わせる。
台帳は `trade`（CASH / MARGIN_OPEN / MARGIN_CLOSE）列を持ち、資産曲線の合図は**ロング側**の実現損益だけで見る。
立花証券の電文は `wbcore.broker.tachibana`（売買区分 1=売 3=買、返済は建玉番号を個別指定、
51 単元以上の成行売建は事前に拒否）。売建の株数は `selection.PickFrom` が 50 単元で頭打ちにする
（低位株は予算を使い切れないが、拒否されて建てられないよりよい。バックテストも同じ上限）。
気配は `quote_source = "tachibana"` で、ギャップは立花証券の基準値段（前日終値 `pPRP`）で出す
（分割・併合の日にアーカイブの調整前終値で −50% のギャップに見えないため）。
残る未対応: 当日に新たに売り禁になった銘柄（前日公表分しか除外できない。発注時のエラー 11149/11325 で弾かれ、
次の回で再送されない＝機会損失のみ）、張り付きの翌朝の自動返済。

## 気配の取得元（未解決の前提）

取得元は立花証券の時価問合。`daytrade quotes 7203 9984` で疎通を確かめる。

| `quote_source` | 中身 | 用途 |
|---|---|---|
| `tachibana` | 立花証券 e支店 API の時価問合（`CLMMfdsGetMarketPrice`）。寄付後は始値、無ければ現在値 | 本命 |
| `csv` | `symbol,price[,at]` のファイル | 検証・dry-run。別経路で取った気配を流す |

## 設定（`daytrade.toml`）

| 節 | 項目 | 意味 |
|---|---|---|
| `[capital]` | `enabled` | 戦略のオン／オフ。false なら `plan` / `open` は何もしない（`close` は台帳の買いを売る） |
| | `max_capital` | 1 日に使う資金の上限（円）。**0 なら N = 0**: スクリーニングと候補の表示だけ行い買わない |
| | `order_budget` | 1 注文の目安。N の決定に使う（既定 67 万円） |
| | `max_positions` | N の上限（既定 10。研究では N10 超で Sharpe が落ちる） |
| | `weighting` | N 銘柄への配分。`inverse_vol`（既定。20 日ボラの逆数で按分、利益 +8%・Sharpe 1.81→2.02）か `equal` |
| `[universe]` | `segments` | prime / standard / growth。再編前の一部・二部・マザーズも同じ呼び方 |
| | `min_turnover` / `turnover_days` | 売買代金の中央値の下限と日数。上げるほど成績は落ちる |
| | `exclude_cap_terciles` | 時価総額の下位からいくつ外すか（1 が最良） |
| | `exclude_earnings_prev` / `exclude_earnings_today` / `exclude_margin_alert` | 除外条件 |
| `[signal]` | `max_gap` / `min_gap` | ギャップの範囲。既定は「負なら全部」 |
| | `skip_limit_down` | 気配がストップ安の銘柄は買わない（既定 true） |
| `[regime]` | `iv_gate` | 0 で常時。18 で高ボラ局面だけ（DD 半分、稼働 5 割） |
| | `skip_months` | 取引しない月。既定の設定は `[12]` |
| | `drift_days` / `drift_gate` / `drift_gap_override` | 市場の日中ドリフトのゲート（下記）。既定は無効 |
| | `equity_curve_days` / `equity_curve_scale` | 戦略自身の直近 N 日の実現損益が 0 以下なら資金を `scale` 倍に縮める（既定 20 日・0.5）。scale 0 で休む |
| | `us_skip_low` / `us_skip_high` / `us_vix_override` | 前夜の S&P500 が帯の中（既定 0〜+1%）で VIX ≤ 24 なら休む。`us_skip_high` 無しで無効 |
| `[execution]` | `quote_source` / `quote_file` | 気配の取得元 |
| | `entry_window` / `exit_window` | 発注してよい時間帯（JST）。外なら何もしない。`exit_window` の既定は 15:20〜15:30（15:20 の成行はその場で約定、15:25 以降は引け値） |
| | `max_quote_age` | 気配がこれより古ければ使わない（秒） |
| | `kill_switch` | true で発注を止める |

## 危険信号（`[regime]`）

2018 年（−18 万円）と 2021 年（−19 万円）の負けは、**市場（TOPIX）が寄り高・引け安を 1 年続けた**ことによる
（TOPIX の寄り→引けが 2018 年 −7.1 bp/日、2021 年 −3.7 bp/日。戦略の日次損益は TOPIX の日中リターンと相関 0.5）。
買い持ちのデイトレは市場の日中ドリフトを背負う。前日までに観測できる信号を毎朝すべて計算してログ
（`daytrade.regime`）に残し、設定で有効にしたものだけが取引を止める。

| 信号 | 検証（2017–2026、200 万円・N3） | 既定 |
|---|---|---|
| 12 月を休む | 9 年中 7 年の 12 月がマイナス。IS +23 万・OOS +25 万、Sharpe 1.45→1.63 | **有効** |
| 前夜の S&P500 が 0〜+1% の小幅高（VIX ≤ 24） | その翌日は損益 ≈ 0（東証のギャップダウンが個別要因）。休むと Sharpe 1.45→1.63、閾値を動かしても崩れない。12 月休みと併用で合計 667 万・Sharpe 1.85・MaxDD −50 万・全年黒字（IS 1.62 / OOS 2.10） | **有効** |
| 市場の日中ドリフト（TOPIX 寄り→引け 20 日平均 ≤ 0） | 2018 −18→+13、2021 −19→+53 に転じるが、2022 年以降の利益を 3 割削る（IS 限定） | 無効（記録のみ） |
| 資産曲線（戦略の直近 20 日損益 ≤ 0 なら資金を半分） | 休むのではなく縮める。ボラ逆比例の配分と合わせて MaxDD −50→−30 万、利益 −2%、Calmar 1.31→2.14 | **有効** |
| IV（前日 ≤ 18） | CAGR 維持で MaxDD 半分、稼働 5 割 | 無効 |

米国の信号は FRED（`SP500` / `VIXCLS` の終値）を 9:00 の `open` が取りに行く（米国の引けは 6:00 JST。FRED への反映は同日中）。取れなければ
信号なしとして取引する（`daytrade.us_missing`）。
市場ギャップ（候補の中央値ギャップの絶対値 > 1%）の日はドリフトのゲートを無視する。急落・急騰の寄付は
逆張りが最も効く日（+1.7 万円/日）。
ドリフトのゲートを入れる場合は `drift_gate = -0.0003`（−3 bp）を目安に。

## バックテスト

```bash
daytrade backtest --since 2017-01-01          # 設定どおり（資金固定・100 株単位・段階手数料）
daytrade backtest --since 2022-01-01 --trades  # 直近と個別の取引
```

前夜の `plan` と同じ式（`daytrade.universe.eligible_expr`）と 9:00 と同じ順位付け
（`daytrade.select.gap_rank_expr`）をアーカイブのパネルに当てる。検証と実運用で条件が
ずれないための構造。

検証と実運用を揃えるための約束事（`pkg/daytrade/backtest`）:

- **約定モデル**（`FillModel`）: 順位付けと株数は日足の寄付で決め、建値・手仕舞い値だけを差し替えられる。
  既定は寄付で建てて引けで手仕舞う（`OpenCloseFill`、滑りなし）。実運用は 9:01 の成行と 15:20 の成行なので
  寄付・引けとはずれる。分足が入ったら 9:01 の足・15:20 の足を返す実装をここに差し込み、日足の検証には
  測った滑りを bp で入れる（分足の履歴は 2 年しか無く、10 年の検証の置き換えにはならない）
- **資産曲線ゲートの窓**: 実運用（`recentPnL`）と同じく、台帳の定義（約定単価の差 × 数量。現物の手数料は含まない）で、
  止めた日は 0、縮めた日はその倍率で数える。窓に建てた日が 1 日も無ければ判定しない（12 月を休んだ直後の 1 月に
  「損益 0 ≤ 0」で縮めない）
- **張り付き**: 引けが制限値幅に張り付いた取引（売建のストップ高・買建のストップ安）は `margin.carry_penalty` の
  割合で翌寄りに置き換える。両脚とも
- **配分**: 候補が N に満たない日は残った銘柄で総予算（1 注文の予算 × N）を分け合う（`selection.PickFrom` と同じ）。
  売建は 50 単元で頭打ち（成行で出せる上限。`selection.PickFrom` と同じ）
- **母集団**: 売買代金の中央値と 20 日ボラは足が所定の本数揃った銘柄だけ（パネルと前夜の `plan` で同じ）。
  株式分割・併合の日は建てない（前夜に係数を知り得ない）。日次リターンと翌寄りは `AdjFactor` で揃える。
  実運用の 9:00 は立花証券の基準値段（`pPRP`）でギャップを出すので、分割日に −50% のギャップとして候補に乗ることはない
- **縮めた日の取引**（`--trades`）: 株数・損益にその日の倍率を掛けた値。日次の集計と一致する

## cron

`docs/DEPLOY.md` の cron の節を参照。`jquants sync` が前日足を取り込んだ後（20:30）に `plan`、
9:01（再試行 9:04・9:07）と 15:20（再試行 15:24・15:28）に `open` / `close`。`flock` で重複起動を防ぐ。

## ログ

`state/logs/daytrade-<env>.jsonl`。`code` は `docs/LOGGING.md` の「デイトレ」の節。

## 履歴（振り返り・検証用）

台帳は「今日もう建てたか」に答えるための現在の状態で、dry-run の記録は確認のたびに消す。
ログは順位表の上位 N+5 件しか持たず 90 日で消える。日々の振り返りと検証に要る
「何が候補で、何を選び、何を選ばなかったか」は `state/daytrade/history/<種類>/` に
**追記専用の Parquet** で残す。1 回の `plan` / `open` が 1 ファイル
（`<判定日>T<時刻>Z-<run_id>.parquet`）で、同じ日に何度走っても上書きしない
（cron の 9:01 / 9:04 / 9:07 の再試行も、dry-run の確認も全部残る）。

| 種類 | 1 行 | いつ |
|---|---|---|
| `plan` | 母集団の 1 銘柄（`eligible` / `short_eligible` と除外理由の列ごと） | `plan` のたび |
| `plan_meta` | `plan` 1 回の要約（件数・IV・ドリフト） | `plan` のたび |
| `quotes` | 9:00 に受け取った気配 1 銘柄。`usable`（鮮度の検査を通った）と `gap` 付き | `open` が気配を取ったとき |
| `ranking` | 順位表の 1 行。`side`（BUY=ロング / SELL=ショート）、`picked`、`quantity`、`amount` | `open` が順位を付けたとき |
| `open_run` | `open` 1 回の要約。`mode`（live / dry_run / watch）、`outcome`（picked / regime / no_quotes / no_picks / no_capital）、危険信号の値、件数 | `open` が判断まで進んだとき |

全ファイルに `day`（判定日）・`run_id`（ログと同じ）・`recorded_at`（UTC）が付く。
「その日の最終判断」は `recorded_at` が最大の `run_id`（`--latest`）。
「なぜ X が選ばれなかったか」は `ranking` に無ければ `quotes`（ギャップが条件外・ストップ安・気配なし）で追える。

```bash
daytrade history                                  # 種類ごとのファイル数と期間
daytrade history ranking --date 2026-09-03 --latest
daytrade history open_run --from 2026-09-01 --csv /tmp/open_run.csv
```

分析は DuckDB で直接読む（列が増えても古いファイルはそのまま読める）:

```bash
jquants query "SELECT * FROM read_parquet('state/daytrade/history/ranking/*.parquet')
               WHERE picked ORDER BY day, recorded_at"
```

`state/` はホスト固有なので、`state/daytrade/history/` も台帳と同じく別ホストへ同期する（`docs/DEPLOY.md`）。

## 候補の結果と選定の妥当性（`evaluate` / `review`）

建てなかった候補（次点、順位表の残り）がその日どう動いたかを追えないと、選定が妥当か
分からない。`daytrade evaluate` は大引後に朝の順位表の**全行**へ当日の日足を当て、
「建てていたらいくらだったか」を `history/evaluation` に残す（1 回 1 ファイル、上書きしない）。

- 順位表は 9:00 の記録（`ranking_source = quotes`）を使う。**無い日は前夜の plan と当日の
  始値から同じ規則で作り直す**（`archive_open`。バックテストと同じ近似）。発注経路を止めて
  `open` を回していない間でも評価は毎日積める
- 1 行 = 候補 1 銘柄。`rank_group`（picked / next / rest）、始値・終値、`gross_bp`（寄付 → 大引を
  建て方向で見た bp）、`cost_bp`（滑り・貸株料等の見込み）、`net_bp`、`hypo_quantity` / `hypo_pnl`
  （選んだ銘柄は記録の株数、それ以外は予算で買える株数）、`limit_up_close` / `limit_down_close`
  （売建の持ち越しリスク）、`traded` / `actual_pnl`（台帳の本発注の約定）
- `quotes` の日は `gap`（9:00 の気配）と `gap_open`（実際の始値）の差で、気配の当たり具合も見える

```bash
daytrade evaluate --config-dir config/daytrade_margin                 # 今日（cron: 20:20）
daytrade evaluate --date 2026-09-02 --config-dir config/daytrade_margin
daytrade review                                                       # 直近 20 日
daytrade review --from 2026-09-01 --csv /tmp/review.csv
```

`review` は日 × 脚ごとに「選んだ N の平均 net bp」「次点の平均」「候補全体の平均」と想定損益・
実現損益を並べ、期間の合計に「picked が勝った日」「picked が all を上回った日」の割合を出す。
選定が効いていれば picked ≥ next ≥ all の日が多い。逆が続けば、順位付けの規則（ギャップの
小さい順／大きい順）がその相場で効いていない合図。全行は DuckDB で直接読める:

```bash
jquants query "SELECT rank_group, avg(net_bp) FROM read_parquet('state/daytrade/history/evaluation/*.parquet')
               WHERE side = 'SELL' GROUP BY rank_group"
```
