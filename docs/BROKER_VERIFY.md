# 立花証券 e支店 API の検証手順（発注経路を開ける前に）

発注に関わる電文のうち、**実機で 1 度も確認できていないものがある**。この文書は
何が未検証で、UAT（demo-kabuka）で何をどの順に確かめるかを記す。ここが終わるまで
`deploy/crontab.txt` の「発注経路」の行は開けない。

## 未検証の電文

| 電文 | 使うところ | 実装 |
|---|---|---|
| `CLMOrderList` | 注文の照会（約定数量・状態） | [pkg/wbcore/broker/tachibana_orders.go](../pkg/wbcore/broker/tachibana_orders.go) |
| `CLMShinyouTategyokuList` | 信用建玉の一覧・返済する玉の指定 | [pkg/wbcore/broker/tachibana_trade.go](../pkg/wbcore/broker/tachibana_trade.go) |
| `CLMKabuNewOrder`（信用） | 信用新規・返済の発注 | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |
| `CLMStkGetIssueMstKabu` | 売買単位 | [pkg/wbcore/broker/tachibana_orders.go](../pkg/wbcore/broker/tachibana_orders.go) |
| `CLMKabuNewOrder`（逆指値・通常＋逆指値） | ストップの発注（`OrderRequest.WithStop`） | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |
| `CLMKabuCorrectOrder` | 逆指値の条件の訂正（トレーリング） | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |
| `CLMOrderList` の逆指値項目（`sOrderGyakusasi*` / `sOrderTriggerType`） | 発火したかの照会 | [pkg/wbcore/broker/tachibana_orders.go](../pkg/wbcore/broker/tachibana_orders.go) |

残高・現物建玉（`CLMZanKaiSummary` / `CLMGenbutuKabuList`）も**未検証**。Go への移植時に
項目名を取り違えていた（`aCLMKabuZan` / `sGenkinZandaka` などは実在しない）ので、
削除済み Python 実装から移植し直した。

項目名と区分コードの出所は、**削除済みの Python 実装**（`git show ac1eb7a:src/wbcore/broker/tachibana.py`）。
Go への初回移植で推定に頼って多数取り違えたため、そこから写し直してある。

## 設計: 分からないときは必ず止まる

未検証の実装で怖いのは、項目名が違ったときに**空の結果が「異常なし」として通る**こと。

- 手仕舞いの数量が 0 になれば、建玉は返済されず持ち越しになる
- 注文履歴が空になれば、積立の二重買付ガード（`UnrecordedFills`）は素通りする

そこで応答の扱いを次の 2 つに分けてある（[tachibana_response.go](../pkg/wbcore/broker/tachibana_response.go)）。

- **配列のキーがあり、要素が 0 件** → 「該当なし」。正常な答えとして通す
- **配列のキーが応答に無い** → `ErrUnverifiedResponse`。**必ず失敗する**

項目名が違えば最初の 1 回で落ち、エラーに「期待したキー」と「実際に返ってきたキー」が
並ぶので、直す場所がすぐ分かる。

呼び出し側も「照会できなかった」を「異常なし」に倒さない。

- `daytrade close`: 買い注文を照会できないときは**数量を推測して売らない**。通知して異常終了する
  （建っていなかった場合に反対建玉を作らないため）
- `daytrade verify`: 照会できなければ「持ち越しなし」と言わず異常終了する
- `wbjp run`: 板の注文を照会できないとき、発注する回は中止する（二重発注を避ける）
- `accum run`: 照会できない注文は台帳を動かさず保留し、通知する

## 項目名を直す場所

実機の応答と食い違ったら、直すのは**定数だけ**で済むようにしてある。

| 直すもの | 場所 |
|---|---|
| 売買区分・現金信用区分・課税区分・注文状態 | `tachibana_codes.go` |
| 注文一覧／単品照会の項目名 | `tachibana_orders.go` の `fieldList*` / `fieldDetail*` |
| 残高・現物建玉・信用建玉の項目名 | `tachibana_trade.go` の `fieldCash*` / `fieldMargin*` |
| 応答の配列のキー | `tachibana.go` の `*Key` 定数 |

**電文ごとに項目名が違う**ことに注意。注文一覧（`CLMOrderList`）は `sOrder*` の接頭辞が
付き、単品照会（`CLMOrderListDetail`）は付かない。約定数量は一覧が `sOrderYakuzyouSuryo`、
単品が `sYakuzyouSuryou`（末尾の `u` の有無まで違う）。取り違えると 0 として読める。

知らない状態コードは `OrderStatusUnknown` に落ちる。Unknown は `IsTerminal()` が false
なので、**確定していない注文を「終わった」と誤認して台帳から落とすことはない**。
売買区分と現金信用区分は、知らない値を買い・現物に落とさず**エラー**にする。

## UAT での確認順

`WBJP_ENV=uat` で、1 段ずつ結果を見てから次に進む。

```bash
# 0. 認証が通ることと、取得元を確認する
WBJP_ENV=uat wbjp credentials check --env uat
WBJP_ENV=uat wbjp account

# 1. 照会系（発注しない）。ここで ErrUnverifiedResponse が出たら項目名を直す
WBJP_ENV=uat accum orders --check

# 2. 現物を 1 単元だけ発注し、照会で拾えることを確かめる
WBJP_ENV=uat accum run --live --ignore-window
WBJP_ENV=uat accum orders --check      # 状態が SUBMITTED → FILLED に変わるか
```

確認したいのはこの 4 点。

1. `accum orders --check` が `ErrUnverifiedResponse` を出さない（＝項目名が合っている）
2. 発注した注文が照会で見つかり、**約定数量と約定単価が入る**
3. 台帳の「発注済み」の額が、想定額から**約定額（株数 × 約定単価）に置き換わる**
4. `accum orders` の「有効額」が実際に払った額と一致する

信用（`daytrade`）はそのあと。

```bash
# 3. 建玉の照会（発注しない）
WBJP_ENV=uat daytrade status --config-dir config/daytrade_margin

# 4. 1 銘柄だけ建てて、同じ日に返済まで通す
WBJP_ENV=uat daytrade open   --config-dir config/daytrade_margin --live --yes
WBJP_ENV=uat daytrade close  --config-dir config/daytrade_margin --live --yes
WBJP_ENV=uat daytrade verify --config-dir config/daytrade_margin
```

信用で確認したいのはこの 3 点。

1. 建てた玉が `MarginPositions` に現れ、**建玉番号（`sOrderTategyokuNumber`）が取れる**
   （返済の指定に要る）
2. `close` が返済として通る（現物売りになっていない。手数料と受渡が信用のものか）
3. `verify` が「持ち越しなし」で終わる（＝ `close` の数量が建玉と一致した）

```bash
# 5. 逆指値（デモ環境で。信用返済での逆指値はリファレンスに例文が無いので必ず実機で）
#    a. 現物 1 単元を買い、売りの逆指値（条件価格は現在値の −3%、発火後は成行）を置く
#    b. CLMOrderList で sOrderGyakusasiOrderType=1 / sOrderTriggerType=0 で返ることを確かめる
#    c. CorrectStop で条件価格を変え、一覧に反映されることを確かめる
#    d. 取消できることを確かめる
#    e. 信用建玉に対して返済の逆指値（建玉指定つき）が受け付けられるか確かめる
#    f. 発火後に CorrectStop が拒否されること（sResultCode ≠ 0）を確かめる
```

## 既知の制約

- **逆指値は発火後に条件を訂正できない**（通常の値段訂正になる）。期日は最長 10 営業日。
  逆指値の値段は値幅制限内・呼値単位で、銘柄・市場ごとに受付停止があり得る（エラーコード一覧）

- **`CLMOrderList` は当日（＋繰越）分しか返らない。** 前日以前の注文は照会できないので、
  積立の `UnrecordedFills` が捕まえられるのは当日の再実行までとなる
- **注文番号は二重発注の防止には使えない。** 発注が受理されて初めて返るので、
  「送ったか分からない」瞬間には手元に無い。防止は `client_order_id` と
  発注前の台帳記録で行い、注文番号は事後の照会・取消に使う
- 空売り価格規制により、**51 単元以上の信用新規売りは成行で出せない**（発注前に弾く）

## 本番へ移すときの条件

上の 7 点がすべて確認できるまで、`deploy/crontab.txt` の発注経路の行は開けない。
開けるときも **まず `--live` 無しで数日**、次に `--live` の順にする。

停止は `config/<戦略名>/settings.toml`（`daytrade.toml`）の `kill_switch = true`。
cron を消さなくても次のサイクルから発注しなくなる。
