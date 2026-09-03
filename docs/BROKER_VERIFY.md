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

現物の残高・建玉・新規注文・取消（`CLMZanKaiSummary` / `CLMGenbutuKabuList` /
`CLMKabuNewOrder`（現物）/ `CLMKabuCancelOrder`）は移植前から通っていた経路。

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
| 注文一覧のキー・項目名 | `tachibana_orders.go` の `orderListKey` / `field*` / `fields*` |
| 注文状態コードの対応表 | `tachibana_orders.go` の `tachibanaOrderStatus` |
| 建玉一覧のキー・項目名 | `tachibana_trade.go` の `marginPositionsKey` ほか |
| 売買区分・現金信用区分・課税区分 | `tachibana_trade.go` の `tachibanaSide*` / `kubun*` / `zei*` |

数量・価格の項目は電文ごとに名前が揺れるので、`fieldsFilledQty` のように**候補を並べて**
先に見つかったものを使う。実機で確定したら 1 つに絞ってよい。

知らない状態コードは `OrderStatusUnknown` に落ちる。Unknown は `IsTerminal()` が false
なので、**確定していない注文を「終わった」と誤認して台帳から落とすことはない**。

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

1. 建てた玉が `MarginPositions` に現れ、**建玉番号が取れる**（返済の指定に要る）
2. `close` が返済として通る（現物売りになっていない。手数料と受渡が信用のものか）
3. `verify` が「持ち越しなし」で終わる（＝ `close` の数量が建玉と一致した）

## 本番へ移すときの条件

上の 7 点がすべて確認できるまで、`deploy/crontab.txt` の発注経路の行は開けない。
開けるときも **まず `--live` 無しで数日**、次に `--live` の順にする。

停止は `config/<戦略名>/settings.toml`（`daytrade.toml`）の `kill_switch = true`。
cron を消さなくても次のサイクルから発注しなくなる。
