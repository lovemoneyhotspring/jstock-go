# 立花証券 e支店 API の検証手順（発注経路を開ける前に）

発注に関わる電文のうち、**実機で 1 度も確認できていないものがある**。この文書は
何が未検証で、UAT（demo-kabuka）で何をどの順に確かめるかを記す。
`daytrade` の発注経路は 2026-09-15 に本番（`--live`）へ切り替えた（`deploy/crontab.txt` の「有効化の段階」。
残りは下の「未検証の電文」のとおり、承知のうえで本番で確かめている）。`wbjp` / `accum` の発注経路の行はまだ開けていない。

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
行が 1 つでもある場合の項目名（約定数量・建玉番号など）は、実際に注文を出すまで確かめられない
——同日昼（現物）と 2026-09-14（信用）に確かめた（下の 2 節）。

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
| `CLMKabuNewOrder`（逆指値の**発火**） | ✅ 条件を現在値の上（1,028 円）に置いて即発火。成行で約定（997 円）・`発火 true` を読めた |
| 発火後の訂正 | ✅ 拒否された（`sResultCode=12050`「通常注文の逆指値条件は訂正できません」） |
| `CLMKabuNewOrder`（通常＋逆指値） | ✅ 指値 1,028 円（約定しない水準）＋条件 969 円で受理。訂正・取消も通った |
| `CLMAuthLoginAck` の口座区分 | ✅ `sSinyouKouzaKubun = 0`。**信用取引口座は今日も未開設** |

確認したかった現物の 4 点はすべて取れた。台帳の投下額は想定 999 円 → 約定額 998 円に
置き換わり、有効額も 998 円。残高は 3,000 → 1,925 円（= 手数料込み 1,075 円を引いた額）で一致。

逆指値は**発注・照会・訂正・取消・発火・発火後の訂正の拒否・通常＋逆指値**まで全部通った。
`accum verify-stop` の `--fire`（発火させる。1 単元が実際に売れる）と `--also-limit`
（通常＋逆指値）で再現できる。

**逆指値の訂正で 2 つ直した。**

1. 応答の文字列に**生のタブ**が入る（`sResultText":"逆指値注文値段変更がありません<TAB>"`）。
   Go の `json.Unmarshal` は文字列中の 0x20 未満を拒否するので、応答全体が読めなかった。
   **拒否理由が載る応答ほど起きる**ので、直さないと「なぜ拒否されたか」が永久に分からない。
   パース前にエスケープする（`escapeRawControls`）
2. 訂正電文の発火後の値段に `"0"`（成行）を入れていた。元から成行の逆指値では
   「変更が無い」と見なされ拒否される（`sResultCode=12115`）。変えないなら `"*"` で送る

**検証そのものが見つけたバグをもう 1 つ直した。** 同じ日に同じ内容で `verify-order` を 2 回叩くと
`client_order_id` が一致し、**ブローカーには 2 件出るのに台帳は 1 行のまま**上書きされた。`accum run` と同じ柵
（`WasPlaced`）を `verify-order` にも置いた（もう 1 単元出したいなら `--units 2` のように数量を変える）。

**この日の検証で使った額は手数料 231 円（77 円 × 3 回）。** 563A 1 株（998 円）は保有したまま。

口座区分はどの CLI にも出していなかったので、調べもののプローブを足した。

```bash
WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
  TACHIBANA_KOUZA_PROBE=1 go test ./pkg/wbcore/broker -run TestKouzaProbe -v
```

`go test` は .env と秘密鍵の相対パスを解決できない（作業ディレクトリがパッケージの側になる）ので、
上のように絶対パスで渡す。

信用新規・信用建玉の行あり・信用返済の逆指値は、2026-09-14 に信用口座が開いてから確かめた（下の節）。

## 一般信用の在庫は API では取れない（2026-09-14、本番口座・照会のみ）

優待クロス（つなぎ売り）を自動化できるかは「一般信用の売建可能数量が API で取れるか」で決まる。
**取れない。** 参照系 2 電文を本番口座に投げて確認した
（`TACHIBANA_MARGIN_PROBE=3197 go test ./pkg/wbcore/broker -run TestMarginProbe -v`）。

| 電文 | 返る項目 | 一般信用の在庫 |
|---|---|---|
| `CLMStkGetIssueMstKabu`（銘柄マスタ） | **14 項目だけ** | 無い |
| `CLMZanKaiSummary`（余力） | 62 項目 | 無い（口座単位の金額のみ） |

銘柄マスタの全項目はこれだけで、**貸借区分も一般信用の可否も入っていない**:

```
sIssueCode sIssueName sIssueNameEizi sIssueNameKana sIssueNameRyaku
sBaibaiTani sBaibaiTaniYoku sBaibaiTeisiC sGyousyuCode sYusenSizyou
sDaiyouHyoukaTanka sHosyoukinDaiyouKakeme sTokuteiF sZyouzyouHakkouKabusu
```

余力側で信用に関わるのは `sLargeUridateYoryoku` / `sMiniUridateYoryoku`（売建余力・金額）、
`sSinyouSinkidate`（信用新規建可能額）などで、**どれも口座単位**。銘柄ごとの在庫は無い。

ついでに分かったこと: **`CLMStkGetIssueMstKabu` は `sIssueCode` を渡しても全銘柄（4,448 行）を返す。**
先頭は 1301（極洋）で、指定した銘柄ではない。既存の実装が全件から引いているのはこのため。

結論として、優待クロスの自動化はこの API の上では成立しない（在庫は Web 画面にしか無い）。
記録: `~/obsidian-vault/20-research/2026-09-jp-institutional-arb-scan.md`

## 信用の発注経路を 1 周させた（2026-09-14 昼、本番口座・信用 1 単元）

信用口座が開いた（`sSinyouKouzaKubun = 1`）ので、ザラ場中に 1 単元だけ信用で買い建て、
同じ実行の中で返済まで通した。貸借銘柄で値動きの小さい ETF（単元 10 株・約 2,350 円）を使った。

```bash
WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
  TACHIBANA_MARGIN_ORDER_PROBE=<銘柄> go test ./pkg/wbcore/broker -run TestMarginOrderProbe -v -count=1
```

**実際に発注する**（[tachibana_margin_order_probe_test.go](../pkg/wbcore/broker/tachibana_margin_order_probe_test.go)）。
1 単元・見積り 5,000 円まで・その銘柄に建玉があれば送らない・指値は約定する側の気配に置く。
途中で落ちても、建った玉は最後に返済を試みる。
`TACHIBANA_MARGIN_ORDER_SIDE=sell` で売建（空売り → 返済買い）、`TACHIBANA_MARGIN_ORDER_TYPE=market` で
新規・返済とも成行（`daytrade open` / `close` と同じ種別）になる。

同じ日に 3 通り回し、**3 回ともすべての段が ✅** だった。

| 回 | 新規 → 返済 | 種別 | 建玉の数量 | 返済の逆指値 |
|---|---|---|---|---|
| 1 | 買建 → 返済売り | 指値 | +10 | 売り・気配 −3%（訂正でさらに下） |
| 2 | **売建（空売り）→ 返済買い** | **成行** | **−10**（売建は負の約束どおり） | **買い・気配 +3%**（訂正でさらに上） |
| 3 | 買建 → 返済売り | **成行** | +10 | 売り・気配 −3% |

以下の表は 1 回目の電文ごとの結果。2・3 回目も同じ段がすべて通り、照会では
`種別 MARKET`・`売買 SELL/BUY`・`取引 MARGIN_OPEN/MARGIN_CLOSE` がそれぞれ正しく読めた。

| 電文 | 結果 |
|---|---|
| `CLMZanKaiSummary`（余力） | ✅ 信用新規建可能額 `sSinyouSinkidate` が実数で読めた。**`sLargeKaidateYoryoku` などの建余力は 0 のまま**（使っていない） |
| `CLMKabuNewOrder`（信用新規買い・指値） | ✅ 受理。単品照会で `取引 MARGIN_OPEN`・FILLED・約定単価が読めた |
| `CLMShinyouTategyokuList`（行あり） | ✅ `sOrderTategyokuNumber`（建玉番号）・`sOrderHensaiKanouSuryou`・`sOrderTategyokuTanka`・`sOrderTategyokuDay` が現行の定数どおり。`MarginPositions` の建玉番号も取れた |
| `CLMKabuNewOrder`（信用返済の逆指値・建玉指定つき） | ✅ 受理。照会で `取引 MARGIN_CLOSE`・条件・`発火 false` が読めた（手順 5 e） |
| `CLMKabuCorrectOrder`（返済の逆指値の条件） | ✅ 条件の訂正が照会に反映された |
| `CLMKabuCancelOrder`（返済の逆指値） | ✅ CANCELLED |
| `CLMKabuNewOrder`（信用返済売り・指値） | ✅ 受理 → FILLED（`取引 MARGIN_CLOSE`・`売買 SELL`）。**現物売りになっていない** |
| 返済後の `CLMShinyouTategyokuList` | ✅ 0 行 |

建玉の行で分かったこと:

- 買建の `sOrderBaibaiKubun` は **`3`**（`ParseSide` がそのまま読めた）
- `sOrderTategyokuKizituDay`（期日）は建日の約 6 か月後。制度信用（`sGenkinShinyouKubun = 2`）で建っている
- 定数に無い項目: `sOrderBensaiKubun`・`sOrderTateTesuryou`（建手数料 0）・`sOrderKanrihi`・`sOrderZyunHibu`・`sTategyokuDaikin` など

成行の空売り（10 株）は空売り価格規制の適用除外（50 単元以内）で、そのまま受理された。

**余力の動き**: 1 回目のあと、現物買付可能額が 1 円減った（= 気配の差 0.1 円 × 10 株）。
2・3 回目の成行はそれぞれ 0.2 円 × 10 株 = 2 円の損で、3 回の合計は 5 円。信用の売買代金に新規と返済の 2 件ぶんが載り、
現物の注文件数は 0 のまま。**信用の手数料は 0 円**で、受渡も信用として処理された。

**返済の単品照会（`CLMOrderListDetail`）に決済の明細が載る。** `aKessaiOrderTategyokuList` の 1 行に
`sKessaiSoneki`（決済損益、例 `-2`）・`sKessaiTateTesuryou`・`sKessaiKanrihi`・`sKessaiKasikaburyou`・
`sKessaiGyakuhibu`・`sKessaiKakikaeryou` などが入り、この日は損益以外すべて 0 だった（日計りなので当然）。
新規の単品照会には無い。返済の約定代金の欄 `sGaisanDaikin` は損益（`-2`）で、新規は約定代金（`2345`）。
信用の損益を台帳で持つなら、ここから引ける。

余力の電文で使えたもの（照会のみ）: `CLMZanRealHosyoukinRitu` は当日（`sT0*`）と 5 営業日後（`sT5*`）の
差入保証金・受入保証金・評価損益・委託保証金率を返す（`sT0HyoukaSonEki` が当日の損益）。
`CLMZanKaiKanougaku` は現物買付可能額だけ、`CLMZanShinkiKanoIjiritu` は信用新規建可能額だけ。
`CLMZanKaiSinyouSinkidateSyousai` はザラ場中に `991002`（一時的に利用不可）で返った。

> **現物買付可能額が、テストの損 5 円とは別に 1,000 円減った**（1 回目のあと、12:44 → 12:57 のどこか。
> 2・3 回目の空売り・成行のどちらかで付いた）。入出金・取引履歴に 1,000 円の動きは無い（ユーザーが Web で確認）。
>
> **正体は「その他拘束金」（`sSonotaKousokukin` = 1,000）で、お金は減っていない。**
> `CLMZanKaiKanougakuSuii`（買付可能額の推移、6 営業日ぶん）で見ると:
>
> | 日付 | 預り金 `sAzukariKin` | その他拘束金 | 日計り拘束金 `sHibakariKousokukin` | 現物買付可能額 |
> |---|---|---|---|---|
> | 当日・翌営業日 | 決済損 5 円の受渡前 | 0 | 0 | 決済損 5 円は `sHosyoukinHikidasiKousokukin` に載る |
> | 受渡日（+2）以降 | 5 円減った額 | **1,000** | 0 | 預り金 − 1,000 |
>
> 立花の Q&A にある「日計り取引拘束金（受渡日まで計上）」は `sHibakariKousokukin` の方で、**0 だった**。
> その他拘束金は受渡日から表示範囲の最後（+6 営業日）まで 1,000 円のまま続き、何の拘束かは API にも
> Q&A・取引ルールのページにも書かれていない（2026-09-14 時点）。**消えなかった**——2026-09-16 の照会では
> 2026-09-18 の行に 3,211 円へ増えていた（下の「委託保証金の内訳」）。正体は未解決で、立花証券のサポートへの問い合わせが残っている。
>
> 内訳の電文: `CLMZanKaiGenbutuKaitukeSyousai`（`sHitukeIndex` 0〜5 で日付ごと）は、ザラ場中は
> 0〜2（当日〜+2）が `991002`（一時的に利用不可）で、3〜5 だけ返った。`CLMZanKaiKanougakuSuii` は 6 日ぶんを
> 配列（`aKanougakuSuiiList`）でまとめて返し、ザラ場中でも全部取れた。**拘束を調べるならこちら。**

**逆指値だけの注文は、発火待ちの間 `PENDING` で読まれる**（現物の逆指値でも同じ）。`PENDING` は台帳側で
「送信結果不明」の意味にも使うので、逆指値を台帳に載せる経路を作るときは区別が要る。

**まだ確かめていないこと**（下の「未検証」に残した）:

- `daytrade open` / `close` / `verify` を通した経路（台帳への記録・close の数量・verify の「持ち越しなし」）
  → **確認済み（2026-09-16）**。本番で 4 件を建て、15:20 の close のあと verify が `carried: 0`（持ち越しなし）で終わった。
  プローブは電文だけを直接叩いていたので、台帳を通した 1 周はこれが最初
- 前営業日の注文を `CLMOrderListDetail` で照会できるか。今日の新規・返済の注文番号は日誌
  （`~/obsidian-vault/10-journal/2026-09-14.md`）に残したので、翌営業日に照会する（照会だけ・発注しない）:

  ```bash
  WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
    TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
    TACHIBANA_ORDER_DETAIL_PROBE=<注文番号/営業日>,... go test ./pkg/wbcore/broker -run TestOrderDetailProbe -v -count=1
  ```
- 寄付の執行条件（`sCondition = 2`）＝寄成と、注文値段区分（`sOrderOrderPriceKubun`）の 3 / 4 の読み
  → 下の「未検証の電文」の表の下へ寄せた

## 委託保証金の内訳（2026-09-16、本番口座・照会のみ）

建玉の上限を保証金から導けるかを確かめた（`pkg/daytrade/margincap`）。**導ける。**

| 電文 | 結果 |
|---|---|
| `CLMZanKaiSummary` の `sOhzs*` | ❌ **建玉ゼロだと全項目が空文字**。日計りでは常に空なので保証金の取得には使えない |
| `CLMZanKaiKanougakuSuii` | ✅ 6 営業日ぶんの受入保証金・現金保証金・代用評価額・建可能額・その他拘束金。ザラ場中でも全部返る。**本番はこれを使う** |
| `CLMZanRealHosyoukinRitu` | ✅ 受入・差入保証金と追証余力（当日 `sT0*` と T+5 の断面） |
| `CLMZanShinkiKanoIjiritu` | ✅ 信用新規建可能額だけ |

> **不足額（`sFusokugaku`）の項目名はこの電文で確認できていない。** 上の ✅ に並ぶのは
> 受入保証金・現金保証金・代用評価額・建可能額・その他拘束金の 5 つで、追証の情報を実機で
> 見たのは別電文（`CLMZanRealHosyoukinRitu` の「追証余力」）の方。`CLMZanKaiKanougakuSuii` に
> `sFusokugaku` が実在するかは未検証で、**無ければ欠損が 0 と読まれ「追証の日は建てない」
> （`margincap.Apply`）は一度も発火しない**。2026-09-17 に、項目が無いことを
> `MarginSummary.Missing` に残し `daytrade.margin_warm` で警告するようにした——
> 朝のログに `missing` が出たら、この項目名が違う。確かめるには追証の出ている口座か、
> `aKanougakuSuiiList` の 1 行をそのまま出して項目名を並べる。

実測値: 受入保証金 3,813,827 = 現金保証金 2,093,827 + 代用有価証券評価額 1,720,000。
**代用の掛目は証券会社が掛けた後の値が返る**ので、現金と代用の按分を自分で持つ必要はない。
信用新規建可能額 11,557,051 は 受入保証金 ÷ 0.33 = 11,556,445 と 606 円差で、
`backtest.RequiredMargin` の保証金率 33% は実口座と一致する。

代用掛目は銘柄マスタ側にあり、ETF 5 本（1305 / 1629 / 1655 / 2559 / 563A）とも一律 80% だった。

> **`CLMStkGetIssueMstKabu` は `sIssueCode` を渡しても絞り込まれず全 4452 行を返す。**
> `tachibana_margin_probe_test.go` は `rows[0]`（先頭＝1301 極洋）を読むので、
> **銘柄別の値を見たつもりで先頭行を見ることになる**。銘柄で絞るなら返った行を自分で突き合わせること。

**その他拘束金 `sSonotaKousokukin` は未解決のまま。** 2026-09-14 に 1,000 円、2026-09-16 の照会では
2026-09-18 の行に 3,211 円と増えている。何の拘束かは API にも Q&A にも書かれていない（上の節も参照）。
保証金から建玉を導くと枠をそのまま削るので、`margincap` は 6 日ぶんの**最小**を採り、拘束金をログに残す。

## 未検証の電文

| 電文 | 使うところ | 実装 |
|---|---|---|
| `CLMOrderListDetail`（前営業日の注文） | 持ち越しの判定（`execute.CarriedPositions`） | [pkg/wbcore/broker/tachibana_orders.go](../pkg/wbcore/broker/tachibana_orders.go) |
| `CLMKabuNewOrder` の `sCondition = 2`（寄成） | 寄る前の発注（`execution.preopen_legs`。**2026-09-19 から `long`**・最初の営業日は 9/24。確かめる 2 点は表の下） | [pkg/wbcore/broker/tachibana_codes.go](../pkg/wbcore/broker/tachibana_codes.go) `conditionCodeOf` |
| 板寄せに**間に合わなかった**寄成（9:00:00 以降に届いた `sCondition = 2`）の行き先 | 寄成の締め切りは 9:00:00 ちょうど（`RunDeadline`）。気配の 1 本が遅れると 8:59:59 台に出る（2026-09-21 から、締め切りまで 1 秒を切ったら送り始めない。`execute.EntrySendMargin`）。拒否されるのか、後場寄り（12:30）の板寄せに回るのかを確かめる——後場寄りに回るなら検証していない時刻の建玉になるので、送信の締め切りを 8:59:57 へ詰める。close は板に残った建て注文を取り消す（`RefreshEntries`）ので持ち越しにはならない | [pkg/daytrade/config/config.go](../pkg/daytrade/config/config.go) `RunDeadline` |
| `CLMKabuNewOrder` の **指値 × `sCondition = 2`（寄指）** | 寄る前のロングを寄指にする（`execution.preopen_limit_pct`。平常日は既定 0 のままで出ていない。米国小幅高の日だけ `preopen_limit_pct_us_low = 1.5` が 2026-09-21 から有効）。手順は下の「寄指のプローブ」 | [pkg/daytrade/execute/execute.go](../pkg/daytrade/execute/execute.go) `OpeningLimitPrice` / `EntryRequest` |
| `CLMKabuNewOrder` の `sCondition = 4`（引け）× 信用返済 | **保険の手仕舞い**（`execution.protect_exit`。**2026-09-20 から有効**・本番で検証中）。手順は下の「引けの保険注文」 | [pkg/wbcore/broker/tachibana_codes.go](../pkg/wbcore/broker/tachibana_codes.go) `conditionCodeOf` |
| 引けの保険が**一部約定**したときの `CLMOrderList` / `CLMOrderListDetail` の返り方 | 保険の聞き直しで「約定数量が減って見えたら台帳へ書き戻さない」保護の前提（一部約定後の取消・失効で約定数量欄に約定分が載るか。2026-09-20 のレビューで未確認と記録） | [pkg/daytrade/execute/protect.go](../pkg/daytrade/execute/protect.go) |
| 時価問合の `p_no` 検査が**セッション単位か口座単位か** | 番号順にずらして送る途中で再ログインが入ったときだけ効く（弾かれても 2 周目が直列で取り直す）。実験は下の「時価問合を並列に送れるか」 | [pkg/wbcore/broker/tachibana_price.go](../pkg/wbcore/broker/tachibana_price.go) |
| 発注と照会の**合計 4 回/秒**（`orderLimiter` 2 + `requestLimiter` 2）が上限に当たらないか | 注文の枠は上げていない（実機の上限を確かめていない）。9/24 の朝に `broker.request_failed` が出ないか見る | 同 `requestLimiterFor` |

信用建玉（行あり）・信用新規／返済の発注・信用返済の逆指値は 2026-09-14 に本番口座で確認済み（上の節）。
`daytrade` の台帳を通した信用の 1 周（`open` → `close` → `verify`）は 2026-09-16 に本番で確認済み（4 件を建て、close のあと verify が `carried: 0`）。

残高・現物建玉（`CLMZanKaiSummary` / `CLMGenbutuKabuList`）は 2026-09-11 に実数で確認済み。

項目名と区分コードの出所は、**削除済みの Python 実装**（`git show ac1eb7a:src/wbcore/broker/tachibana.py`）。
Go への初回移植で推定に頼って多数取り違えた（`aCLMKabuZan` / `sGenkinZandaka` などは実在しない）ため、そこから写し直してある。

### 寄付の執行条件（`sCondition = 2`）＝寄成

実装したが実機で出していない。`execution.preopen_legs` は 2026-09-19 から `long`（コードの既定は `none`）で、
最初の営業日の 2026-09-24 に 8:59:50 の回が初めて出す。それまでの本番は `sCondition = 0`（条件なし）の成行を
時間帯の中で出していて、プローブと同じ電文。デモ環境（`demo-kabuka.e-shiten.jp`）では確かめていない
（承知のうえで開けた。[DAYTRADE.md](DAYTRADE.md) の「まだ確かめていないこと」）。本番で次の 2 つを見る:

1. **電文が通るか。** `sOrderPrice = 0` × `sCondition = 2` の組で受け付けられるか。銘柄・市場に
   よっては拒否される（エラー「商品市場別設定.執行条件寄付不可」）。51 単元以上の信用新規売りは
   成行で出せないので、寄成も同じ規制に掛かるはず（下の「既知の制約」）。拒否なら台帳に REJECTED が残り、
   9:00:03 からの回が従来のザラ場成行で埋める
2. **寄らなかったときにいつ失効するか。** 前場中に一度も寄らない銘柄（ストップ高の張り付き）で、
   注文が前場引けで失効するのか、大引けまで残るのか、後場の寄付の板寄せに参加するのか。
   リファレンスに記述が無い。**ここが分かるまで「寄らない銘柄の枠をいつ諦めるか」は決められない**
   （取引所が失効させるなら何もしなくてよく、残るならこちらから取消を送ることになる）

執行条件のコード（リファレンス v4.5/v4.10）は **0 指定なし / 2 寄付 / 4 引け / 6 不成**。
引け（4）は保険の手仕舞いで使う（下の「引けの保険注文」）。不成（6）は使う予定が無い。

### 注文値段区分（`sOrderOrderPriceKubun`）の 3 / 4 の読み

**仕様と食い違っている。** `tachibana_codes.go` の `orderTypeFromCode` は 3 を成行、4 を指値と読む（移植元の Python は「引け成行」「引け指値」と書いていた）が、
リファレンスの CLMOrderList では **1 成行 / 2 指値 / 3 親注文より高い / 4 親注文より低い**
（逆指値の親子関係）。引けは執行条件（`sCondition = 4`）の側に出るはず。
成行・指値として読む分には大きく外れないので値は変えていないが、注文照会の実データで確かめる。
影響しうるのは `tachibana_orders.go` の「`priceKubun` が 2 か 4 のときだけ `LimitPrice` を採る」判定。

## 引けの保険注文（`execution.protect_exit`）を実機で確かめる

**何のためか。** 建てた玉に、執行条件「引け」（`sCondition = 4`）の返済・売りをブローカーへ先に置く
（`daytrade protect`、**後場だけ** 12:31・13:20・15:10。`execution.protect_window`）。注文は立花に残るので、cron・マシン・ネットが止まっても
引けで手仕舞われる。人が気づいて端末を打つ前提にしない安全網。ふだんの手仕舞いは 15:20 の成行のまま
（引け値は 15:20 より両脚とも不利。分足の検証で合算 +525 万 → +470 万）で、close が保険を取り消してから
成行を出す。**2026-09-20 に有効にした（ユーザ判断。検証しながら本番で回す。最初の営業日は 9/24）。実機で確かめていないこと:**

1. 信用返済（`sTatebiType = 1`・建玉個別指定）に `sCondition = 4` が通るか。拒否なら `protect` が通知を 1 通出し、
   その日はもう置かない。売買は止まらない（close は従来どおり 15:20 に手仕舞う）
2. 保険が**場中に約定してしまわないか**（引けの条件を成行と読み違える）。置いた直後に約定したら、
   その日の持ち時間が変わる（利益の源泉は寄り後〜15:20）。台帳・立花の約定一覧で確かめる
3. 保険の取消が通り、返済できる建玉（建玉一覧の返済可能株数 `sOrderHensaiKanouSuryou`）が**元に戻る**か。
   戻らないと 15:20 の成行が「返済できる建玉が 0 株」で拒否され、保険に任せる形になる（引け値で手仕舞い。
   通知は出る）。取消の直後に建玉一覧を見て、返済可能株数が戻っているか確かめる
4. 引けで約定した保険が、台帳（`queryFill`）と翌朝の持ち越し判定（`CarriedPositions`）で「手仕舞い済み」に
   なるか
5. **前場に出した「引け」が前引け（11:30）で執行されないか**（2 は「場中に約定しないか」までで、前引けを名指ししていなかった。
   立花の仕様は未確認）。9/24 の 11:30 過ぎに約定一覧を見る。執行されるなら建玉が昼に手仕舞われるので、保険を置く回を
   後場（12:30 以降）だけにするか、受け入れるかを決める。急ぎで止めるなら `protect_exit = false`（bin・cron の作り直し不要）
   - **結果（2026-09-24）: 執行される。** 9:20 に置いた 4 本が全部 11:30:00 に約定した（CLMOrderListDetail の
     `aYakuzyouSikkouList[].sYakuzyouDate = 20260924113000`）。前引けの手仕舞いは 15:20 の成行より 1 日 −4,758 円（t −2.62、
     vault `20-research/2026-09-jp-daytrade-exit-midday`）なので、**保険は後場だけ**にした（`execution.protect_window = ["12:30", "15:19"]`、
     cron は 12:31・13:20・15:10）。前場（9:00〜12:30）にマシンが止まると保険は無い

**確かめ方（1 単元・1 日）。**

```bash
# 1. 有効（config/daytrade/daytrade.toml の protect_exit = true。cron が読む設定なので build は要らない）
#    戻すなら false にするだけ
# 2. 12:31 の cron の後（または手で。後場だけ）:
WBJP_ENV=prod ./bin/daytrade protect --config-dir config/daytrade_margin --live --yes
./bin/daytrade status --config-dir config/daytrade_margin   # 建玉と手仕舞いの注文
# 3. 立花の注文一覧で 引け・返済の注文が「受付済み」で残っていること（約定していないこと）
# 4. 15:20 の close の後: 保険が「取消済み」、成行の返済が約定していること
# 5. 15:40 の verify が持ち越しなしで終わること
```

有効にした最初の日に `daytrade.protect`（発注）・`daytrade.protect_cancel`・`daytrade.protect_held`
（取消を確かめられず保険に任せた）のログと通知が出る。出なければ何も起きていない。

## 寄指のプローブ（`execution.preopen_limit_pct`）を実機で確かめる

**何のためか。** 寄る前の回のロングを、寄成でなく寄指（指値 = 前日終値 × (1 − x%)、執行条件「寄付」）で出す。
始値が指値より下のときだけ**始値で**約定するので、「8:59 台の気配では深く見えたのに浅く寄った銘柄」を買わずに済む
（[DAYTRADE.md](DAYTRADE.md) の「寄指にする」、vault `20-research/2026-09-jp-daytrade-limit-on-open.md`）。
**先に 9/24 以降の寄成（成行 × `sCondition = 2`）が通っているのを確かめてから**にする。実機で確かめていないこと:

1. **電文が通るか。** `sOrderPrice = 指値` × `sCondition = 2` × 信用新規買い。拒否なら台帳に REJECTED が残り、
   9:00:03 からの回が従来のザラ場成行で埋める（寄成が拒否されたときと同じ戻り方。通知も出る）
2. **約定値が指値でなく始値か。** 台帳の約定単価 × 日足の始値（`evaluate` の `ref_price` 列）。指値で約定していたら
   寄付条件が効いていない
3. **指値に届かなかった注文がいつ・どの状態で終わるか。** 板寄せの直後に失効（`sOrderStatusCode = 12` → `EXPIRED`）か、
   板に残って後場の寄付にも参加するか、大引けまで残るか。残るなら 15:20 の `close` が取り消す（`RefreshEntries`）が、
   **後場寄り（12:30）で約定しうる**なら持ち時間が変わるので、前場のうちに手で取り消す運用か、
   前場に回る取消の回を足すかを決める（`protect` は後場だけになったので、12:30 より前の取消には使えない）
4. **余力がいつ戻るか。** 失効・取消の後に信用新規建余力が戻ること（その日はもう建てないので実害は無いが、翌朝に残らないこと）
5. 始値 = 指値ちょうどのときの約定（板寄せでは保証されない。頻度は 1〜2%。模擬は「約定しない」と置いた）

**確かめ方（1 日）。**

```bash
# 1. bin を作り直す（preopen_limit_pct を知らない古い bin は strict mode で全コマンドが止まる）
deploy/build.sh && WBJP_ENV=prod ./bin/daytrade preflight --config-dir config/daytrade_margin
# 2. config/daytrade/daytrade.toml の preopen_limit_pct = 0.5 のコメントを外す（戻すならコメントに戻すだけ）
# 3. 翌営業日の 8:59:50 の回の後:
./bin/daytrade status --config-dir config/daytrade_margin      # 理由の欄に「（寄指 1234）」、condition = OPENING
# 4. 9:05 ごろ、立花の注文一覧で: 約定した注文の単価 = 始値、届かなかった注文の状態（失効 / 受付済みのまま）
# 5. 15:20 の close の後: ログに daytrade.opening_unfilled（届かなかった件数と銘柄）、15:40 の verify が持ち越しなし
# 6. その日の影の記録と突き合わせる（どの銘柄が約定するはずだったか）
test/.venv/bin/python test/dt_live_shadow.py --since <その日>
```

**2026-09-21 追記: 最初に寄指が出るのは米国小幅高の日。** `execution.preopen_limit_pct_us_low = 1.5` を先に本番へ入れた
（平常日は寄成のまま）。前夜の S&P500 が 0〜+1%・VIX ≤ 24 の翌朝に、上の 3〜6 をその日にやる。この指値は **85% が届かない**ので、
3（届かなかった注文が後場寄りに残るか）が平常日の 0.5% より重い——9:05 に立花の注文一覧で状態を見て、受付済みのまま残っていたら
12:30 より前に手で取り消す。拒否（1）ならその日は休みになるだけ（9:00 以降の回は埋めない）。

結果はこの節に追記し、3 の答えで運用（後場寄りの前に取り消すか）を決める。

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

**`env` では切り分けられない。** 本番口座（`env=prod`）で電文を確かめることがあり、印が無いと、あとからログを読む
`night-repair`（6:00）と `daily-report`（17:35）が、検証で出た「時間外の発注」「持ち越し」を**本当の異常として拾う**。

`--broker-verify` を付けると、その実行のログの**全行**とダイジェストに `verify: true`、台帳の注文に `orders.verify`、
履歴に `open_run.broker_verify` が付く。検証で建てた玉は**本物**なので `close` / `verify` は普段どおり手仕舞い、
外れるのは成績の集計と資産曲線のゲートだけ（詳しくは [LOGGING.md](LOGGING.md) の「`verify`」、
[DAYTRADE.md](DAYTRADE.md) の「実機検証の印」）。

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

1・2 は 2026-09-14（上の「信用の発注経路を 1 周させた」）、3 は 2026-09-16 に本番の `open` → `close` → `verify`
（4 件、`carried: 0`）で確認済み。**残っているのは 4。**

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
# 発火（a 以降の本番）と発火後の訂正（f）は --fire、通常＋逆指値は --also-limit:
#
#   accum verify-stop --symbol 563A --fire --live -y        # **1 単元が実際に売れる**
#   accum verify-stop --symbol 563A --also-limit --live -y  # 約定しない指値＋条件
#
# a〜d・f と通常＋逆指値は 2026-09-11 に本番口座で確認済み。
# e（信用返済の逆指値）は 2026-09-14 に TestMarginOrderProbe で確認済み。
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

現物と逆指値は 2026-09-11、信用の電文（買建・売建、指値・成行の新規と返済、返済の逆指値）は
2026-09-14 に本番口座で通した。
信用の 4 点のうち 3（`daytrade verify` の「持ち越しなし」）は 2026-09-16 に本番で確認した。
**残っているのは 4（前営業日の単品照会）**と、上の「未検証の電文」の執行条件（寄成・寄指・引け）。

`daytrade` の発注経路は、dry-run を 2026-09-11 から回したうえで 2026-09-15 に `--live` へ切り替えた（残りは本番で確かめる判断）。
`wbjp` / `accum` の発注経路の行はまだ開けていない。開けるときも **まず `--live` 無しで数日**、次に `--live` の順にする。

停止は設定の `kill_switch = true`（`config/daytrade/daytrade.toml`・`config/accum/accum.toml`・wbjp は `config/settings.toml`）。
cron を消さなくても次のサイクルから発注しなくなる。

## 時価問合を並列に送れるか（2026-09-21、確認済み）

`CLMMfdsGetMarketPrice` は 1 リクエスト 120 銘柄までで、914 銘柄 = 8 本を直列に送ると 1.1〜1.5 秒かかる。
縮めるために実機で確かめた（注文は出さない。`pkg/wbcore/broker/tachibana_price_parallel_probe_test.go`）。

| 確かめたこと | 結果 |
|---|---|
| 8 本を同時に送る | **通らない**。先に着いた 1 本だけ通り、残り 7 本は `p_errno=6`「引数（p_no:[3] <= 前要求.p_no:[10]）エラー」。立花は**届いた順に** p_no が増えていることを検査する |
| `p_errno=6` のあとのセッション | **生きている**。直後の大きい番号は通る（失効として捨てる必要はない） |
| 応答を待たずに、番号順に間隔を空けて送る | **通る**。960 銘柄 × 8 本が、間隔 50 / 30 / 20 / 10 ms のどれでも 12 回とも 8/8、合計 0.35〜0.58 秒 |
| 送信の上限 | 8 本を 0.35 秒で送る形を 0.3 秒おきに 12 回続けても弾かれない。`priceLimiter` を 4 → 8 回/秒に上げた |
| 実装した経路（`MarketPricesRawPartialAt`） | 914 銘柄で直列 1.14〜1.16 秒 → **0.51〜0.60 秒**、失敗バッチ 0。1 回の中の応答時刻の幅は 0.97 秒 → 0.33〜0.41 秒 |

```bash
WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
  TACHIBANA_PRICE_PARALLEL_PROBE=1 TACHIBANA_PROBE_STAGGER_MS=50,30,20,10 TACHIBANA_PROBE_SYMBOLS=<銘柄,...> \
  go test ./pkg/wbcore/broker -run TestPriceParallelProbe -v -count=1
```

プローブは実験のあいだ（10 秒以上）本番のセッションのロックを握り続ける。**場の時間外・cron の発注系と重ならない時刻に走らせる。**

**残り**: 測ったのは連休の深夜。9:00 台の混んだ回線で到着が入れ替わる頻度は未確認——構造化ログの `broker.price_retry`
（弾かれたバッチを直列で取り直した）が朝に出るかを見る。送った形と間隔は毎回 `broker.price_pipelined`（`stagger_ms`・`failed`・`elapsed_ms`）に残る。頻発するなら cron の行に `TACHIBANA_PRICE_STAGGER_MS=100`、
直列に戻すなら `=0`（bin の作り直しは不要）。
