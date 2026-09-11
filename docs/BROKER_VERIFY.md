# 立花証券 e支店 API の検証手順（発注経路を開ける前に）

発注に関わる電文のうち、**実機で 1 度も確認できていないものがある**。この文書は
何が未検証で、UAT（demo-kabuka）で何をどの順に確かめるかを記す。ここが終わるまで
`deploy/crontab.txt` の「発注経路」の行は開けない。

## 実機で確かめた電文（2026-09-11、本番口座・照会のみ）

口座を開いた日に本番へ繋いで確かめた。**信用取引口座は未開設**（`sSinyouKouzaKubun = 0`）、
残高 0 円の状態なので、返るのはどれも「該当なし」。

| 電文 | 結果 |
|---|---|
| `CLMAuthLoginRequest` | ✅ 仮想URLの復号まで。p_no・p_sd_date の形と、URL 末尾の改行を直した |
| `CLMZanKaiSummary`（残高） | ✅ 現金 0 円・買付余力 0 円 |
| `CLMGenbutuKabuList`（現物建玉） | ✅ 0 件。**該当 0 件は配列ではなく空文字**で返ると分かった |
| `CLMMfdsGetMarketPrice`（時価・板） | ✅ 3,674 銘柄・66 列を 7.3 秒。10 段の板が返る |
| `CLMShinyouTategyokuList`（信用建玉） | ✅ 0 件・エラー無し。**信用口座が無くても照会は通る** |
| `CLMOrderList`（注文照会・履歴） | ✅ 0 件・エラー無し |
| `CLMStkGetIssueMstKabu`（売買単位） | ✅ 4,447 銘柄。**この電文は sResultCode を返さない**——共通の検査が必須にしていたため、売買単位が黙って空になり既定 100 株に落ちていた（`checkResultOptional` で修正） |

**ここで確かめられたのは「キーがあること」と「0 件を正しく 0 件と読むこと」まで。**
行が 1 つでもある場合の項目名（約定数量・建玉番号など）は、実際に注文を出すまで確かめられない。
下の「未検証の電文」はその意味で残っている。

## 実機で確かめた電文（2026-09-11 昼、本番口座・現物 1 株を発注）

入金（3,000 円）後、ザラ場中に `accum verify-order --symbol 563A --live -y` で 1 株だけ買った。

| 電文 | 結果 |
|---|---|
| `CLMZanKaiSummary`（残高） | ✅ 現金 3,000 円・買付余力 3,000 円。**0 円でない実数**で読めた |
| `CLMKabuNewOrder`（現物買い・指値） | ✅ 受理。注文番号が返る |
| `CLMOrderList`（注文照会・行あり） | ✅ PENDING → **FILLED（1/1 約定・約定単価 998 円）**。行がある場合の項目名が初めて確かめられた |
| `CLMGenbutuKabuList`（現物建玉・行あり） | ✅ 563A 1 株・取得単価 1,075 円（手数料込み）が返る |
| `CLMKabuNewOrder`（逆指値・売り） | ✅ 受理。条件 969 円（現在値 −3%）の「逆指値だけ」 |
| `CLMOrderList` の逆指値項目 | ✅ 条件価格・発火フラグを読めた（`sOrderGyakusasi*` / `sOrderTriggerType`） |
| `CLMKabuCorrectOrder`（条件の訂正） | ✅ 969 → 950 円。**2 つ直してから通った**（下記） |
| 取消（`CLMKabuCancelOrder`） | ✅ 状態 CANCELLED |
| `CLMAuthLoginAck` の口座区分 | ✅ `sSinyouKouzaKubun = 0`。**信用取引口座は今日も未開設** |

確認したかった現物の 4 点はすべて取れた。台帳の投下額は想定 999 円 → 約定額 998 円に
置き換わり、有効額も 998 円。残高は 3,000 → 1,925 円（= 手数料込み 1,075 円を引いた額）で一致。

**逆指値の訂正で 2 つ直した。**

1. 応答の文字列に**生のタブ**が入る（`sResultText":"逆指値注文値段変更がありません<TAB>"`）。
   Go の `json.Unmarshal` は文字列中の 0x20 未満を拒否するので、応答全体が読めなかった。
   **拒否理由が載る応答ほど起きる**ので、直さないと「なぜ拒否されたか」が永久に分からない。
   パース前にエスケープする（`escapeRawControls`）
2. 訂正電文の発火後の値段に `"0"`（成行）を入れていた。元から成行の逆指値では
   「変更が無い」と見なされ拒否される（`sResultCode=12115`）。変えないなら `"*"` で送る

口座区分はどの CLI にも出していなかったので、調べもののプローブを足した。

```bash
WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
  TACHIBANA_KOUZA_PROBE=1 go test ./pkg/wbcore/broker -run TestKouzaProbe -v
```

`go test` は .env と秘密鍵の相対パスを解決できない（作業ディレクトリがパッケージの側になる）ので、
上のように絶対パスで渡す。

**信用（`daytrade`）の検証は口座が開くまで進められない。** 信用新規・信用建玉の行あり・
信用返済の逆指値は、どれも口座が要る。

## 未検証の電文

| 電文 | 使うところ | 実装 |
|---|---|---|
| `CLMShinyouTategyokuList` | 信用建玉の一覧・返済する玉の指定 | [pkg/wbcore/broker/tachibana_trade.go](../pkg/wbcore/broker/tachibana_trade.go) |
| `CLMKabuNewOrder`（信用） | 信用新規・返済の発注 | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |
| `CLMKabuNewOrder`（通常＋逆指値） | 指値で出して発火したら切り替わる形（売りの「逆指値だけ」は確認済み） | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |
| 発火後の挙動（発火・発火後の訂正の拒否） | ストップが本当に効くか | [pkg/wbcore/broker/tachibana.go](../pkg/wbcore/broker/tachibana.go) |

残高・現物建玉（`CLMZanKaiSummary` / `CLMGenbutuKabuList`）は 2026-09-11 に実数で確認済み。
Go への移植時に項目名を取り違えていた（`aCLMKabuZan` / `sGenkinZandaka` などは実在しない）ので、
削除済み Python 実装から移植し直してある。

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

### 検証の実行には必ず `--broker-verify` を付ける

**`env` では切り分けられない。** 本番口座（`env=prod`）で電文を確かめることがあり、
そのとき `env` は普段の運用と同じ値になる。印が無いと、あとからログを読む
`night-repair`（4:00）と `daily-report`（21:00）が、検証で出た「時間外の発注」
「持ち越し」を**本当の異常として拾う**。

`--broker-verify` を付けると:

| どこ | 何が付くか | 効き |
|---|---|---|
| ログ（`state/logs/<app>-<env>.jsonl`） | その実行の**全行**に `verify: true` | `jq 'select(.verify \| not)'` で本当の異常だけ見られる |
| ダイジェスト（`state/digest/<env>-<日付>.jsonl`） | `verify: true` | 日次・週次レポートが検証の回を除ける |
| 台帳（`orders.verify`） | 検証で出した注文の行 | 成績の集計と資産曲線のゲートから外れる |
| 履歴（`open_run.broker_verify`） | 実行 1 回の要約 | 後から「あの日は検証だった」と分かる |

検証で建てた玉は**本物**なので、`close` / `verify` は普段どおり手仕舞う（印を理由に
無視したりしない）。外れるのは成績の集計だけ——戦略の判断ではないため。

> **その日の本番の `open` は動かなくなる。** 冪等の判定（「今日もう建てたか」）は
> 台帳の生きている注文を数えるだけで、検証の印は見ない。**検証で建てた玉の上に
> 本番の建玉を重ねない**ための意図した挙動なので、寄り付き前に検証するのは避け、
> 引け後か、その日は取引しないと決めた日にやること。

```bash
daytrade open   --live --yes --broker-verify --config-dir config/daytrade_margin
daytrade close  --live --yes --broker-verify --config-dir config/daytrade_margin
daytrade verify              --broker-verify --config-dir config/daytrade_margin
accum run       --live       --broker-verify
wbjp  run       --live       --broker-verify
daytrade status              # 検証の注文は「（検証）」付きで並ぶ
```

`WBJP_ENV=uat` で、1 段ずつ結果を見てから次に進む。**本番口座で確かめるときも
手順は同じで、`--broker-verify` を必ず付ける。**

```bash
# 0. 認証が通ることと、取得元を確認する
WBJP_ENV=uat wbjp credentials check --env uat
WBJP_ENV=uat wbjp account

# 1. 照会系（発注しない）。ここで ErrUnverifiedResponse が出たら項目名を直す
WBJP_ENV=uat accum orders --check

# 2. 現物を 1 単元だけ発注し、照会で拾えることを確かめる
#
# `accum run` が注文を作るのは月初の入金日と増額日だけなので、検証したい日に
# 注文が出ないことがある（実際 2026-09-11 に踏んだ）。1 単元だけ出す口を別に用意した:
#
#   accum verify-order --symbol 563A --live -y     # 上限 2,000 円。超えたら送らない
#
# 買いだけ・現物だけ・1 単元だけ・指値（成行にしない）。台帳には検証の印が付く。
# 銘柄は「売買単位 × 現在値」が小さいものを選ぶ（2026-09-11 時点: 563A 1,006 円、
# 2621 が 993 円。1629 は単元 10 なので 2,766 円で上限に掛かる）。
WBJP_ENV=prod accum verify-order --symbol 563A --live -y
WBJP_ENV=prod accum orders --check      # 状態が SUBMITTED → FILLED に変わるか
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
WBJP_ENV=uat daytrade open   --config-dir config/daytrade_margin --live --yes --broker-verify
WBJP_ENV=uat daytrade close  --config-dir config/daytrade_margin --live --yes --broker-verify
WBJP_ENV=uat daytrade verify --config-dir config/daytrade_margin --broker-verify
```

信用で確認したいのはこの 4 点。

1. 建てた玉が `MarginPositions` に現れ、**建玉番号（`sOrderTategyokuNumber`）が取れる**
   （返済の指定に要る）
2. `close` が返済として通る（現物売りになっていない。手数料と受渡が信用のものか）
3. `verify` が「持ち越しなし」で終わる（＝ `close` の数量が建玉と一致した）
4. **前営業日の注文を `CLMOrderListDetail`（注文番号 + 営業日）で照会できる**。持ち越しの判定
   （`execute.CarriedPositions`）は、台帳で未確定のまま残った前日以前の注文をこれで照会する。
   一覧（`CLMOrderList`）と同じく当日分しか返らないなら、`verify` が走らなかった日の注文 1 件が
   14 暦日のあいだ毎回「照会できない」と通知され、その間は台帳外の建玉の掃除も止まる。
   確かめ方: 翌営業日に `daytrade verify --date <前日> --broker-verify` を回し、前日の注文の
   約定数量が入ること

```bash
# 5. 逆指値（デモ環境で。信用返済での逆指値はリファレンスに例文が無いので必ず実機で）
#    a. 現物 1 単元を買い、売りの逆指値（条件価格は現在値の −3%、発火後は成行）を置く
#    b. CLMOrderList で sOrderGyakusasiOrderType=1 / sOrderTriggerType=0 で返ることを確かめる
#    c. CorrectStop で条件価格を変え、一覧に反映されることを確かめる
#    d. 取消できることを確かめる
#    e. 信用建玉に対して返済の逆指値（建玉指定つき）が受け付けられるか確かめる
#    f. 発火後に CorrectStop が拒否されること（sResultCode ≠ 0）を確かめる
#
# a〜d は口を用意した。**保有している現物**に対して発火しない水準の売り逆指値を置き、
# 照会・訂正・取消まで順に通して、各段を ✅ / ❌ で並べる:
#
#   accum verify-stop --symbol 563A --live -y     # 条件は現在値 −3%。最後に必ず取消す
#
# 売りだけ・現物だけ・保有数量以内・発火しない水準。新規の買いは出さないので
# お金は使わない。保有が足りなければ**何も送らずに止まる**（柵なので alert は飛ばさない）。
# e（信用返済の逆指値）と f（発火後の訂正）は信用口座が開いてから。
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

残っているのは**信用（`daytrade`）の 4 点と、逆指値の発火後の挙動**だけ。信用は
口座が開設されるまで進められない（2026-09-11 時点で `sSinyouKouzaKubun = 0`）。

上の 8 点がすべて確認できるまで、`deploy/crontab.txt` の発注経路の行は開けない。
開けるときも **まず `--live` 無しで数日**、次に `--live` の順にする。

停止は `config/<戦略名>/settings.toml`（`daytrade.toml`）の `kill_switch = true`。
cron を消さなくても次のサイクルから発注しなくなる。
