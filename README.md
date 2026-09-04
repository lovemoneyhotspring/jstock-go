# jstock-go — 日本株 自動売買システム（立花証券 e支店 API）

立花証券 e支店 API（e_api_v4r10）で日本株（現物・信用）を売買するシステム。3つのパッケージからなる。
足データは J-Quants（日本株）と FRED（判断材料に使う米国の指数）から取り、pandas も yfinance も使わない。

| パッケージ | 役割 | CLI |
|---|---|---|
| `wbcore` | 共通基盤。ブローカー・足データ・指標・認証情報・登録簿の仕組み | — |
| `wbjp` | スイング売買。複数の戦略を差し替え・合成し、差分だけを発注する | `wbjp` |
| `accum` | 積立。ドル平均法＋下落局面での増額。売らない | `accum` |
| `daytrade` | デイトレ。日本株のギャップ逆張りを寄付で買い大引で売る（[docs/DAYTRADE.md](docs/DAYTRADE.md)） | `daytrade` |

`wbjp` と `accum` は互いに import しない。どちらも `wbcore` の部品を組み合わせて動く。

> **⚠️ 免責**
> 本ソフトウェアは投資助言ではありません。自動売買は資産を失う可能性があります。
> 必ず UAT（テスト）環境で十分に検証し、自己責任で利用してください。

---

## 設計の前提

### 1. 足データはブローカーの API から取らない

立花証券 e支店 API にあるのは発注と時価問合（現在値・始値）で、日足の履歴は無い。

→ **価格データ層とブローカー層を完全に分離**し、日本株の足は J-Quants API（公式）、
判断材料に使う米国の指数（S&P500 / VIX / NASDAQ）は FRED から取る。
9:00 の気配（デイトレ）だけは立花証券の時価問合を使う。

### 2. 発注は成行・指値のみ。逆指値は使わない

→ **損切りはエンジン側で合成する**（`wbjp.risk.stops`）。
ストップ価格をローカルに保持し、日足更新時に評価して成行/指値の決済注文を出す。

> **重要な限界**: 日足運用のため、場中の急落にはその日のうちに反応できない。
> 想定より大きな損失が出る可能性がある。この制約を理解した上で使うこと。

---

## 売買する市場と、判断に使う米国の指数

売買するのは東証だけ（`universe.market = "JP"`）。市場ルール（呼値・単元・値幅制限・差金決済の回避）は
`wbcore.domain.market_rules` / `jp_rules` に集約してある。

米国の指数は**判断材料としてだけ**使う（`Market.US` はそのための識別子で、発注には使えない）:

| 用途 | 何を見るか | 取得元 |
|---|---|---|
| デイトレの「米国市場ゲート」 | 前夜の S&P500 の騰落と VIX（`regime.us_skip_*` / `us_vix_override`） | FRED `SP500` / `VIXCLS` |
| 積立の倍率判定 | `signal_symbol = "^GSPC"` / `"^IXIC"` | FRED `SP500` / `NASDAQCOM` |

FRED は終値だけを返す（四本値・出来高は無い。`open` / `high` / `low` は終値で埋める）。
`SP500` は直近 10 年ぶんしか公開されない。銘柄コードは従来どおり `^GSPC` のように書き、
`wbcore.data.fred_provider.SERIES` で FRED の系列 ID に読み替える。

### 手数料

立花証券の**定額コース**を前提にする。現物は 1 日の約定代金**合計**で段階が決まり
（12 万円まで 0 円、20 万円まで 176 円、50 万円まで 253 円、100 万円まで 506 円、以後 100 万円ごとに 253 円）、
信用は 0 円。表は `wbcore.broker.tachibana.FLAT_RATE_TABLE` に 1 つだけ置き、発注前の見積り
（`preview`）とデイトレのバックテスト（`daytrade.fees`）、スイングの検証・dry-run が使う
`PaperBroker` が同じ表を見る（`PaperBroker` は約定のたびに「当日合計が増えたぶんの差分」を取る）。

### 戦略のサンプル（`wbjp`）

`config/strategies.toml` で名前と重みを書いて組み合わせる。`sma_cross` / `rsi_reversion` / `atr_breakout` の
古典 3 本のほか、次の 4 本がある（いずれも `benchmark` に地合いフィルタ用の銘柄を取る。既定の
`SPY` は東証の銘柄に置き換えること——例: `1306`）。

| 戦略 | 狙い | 建て | 降り |
|---|---|---|---|
| `trend_pullback` | 勝率（小さく多く）。上昇トレンド銘柄の押し目からのブレイクアウト | 条件成立日に毎日 | RSI(3) 過熱・SMA20 回復・時間切れ |
| `rsi_pullback` | `trend_pullback` の元版（RSI(3) 押し目買い） | 同上 | 同上 |
| `momentum_rank` | 損益比（大きく少なく）。6 ヶ月リターン÷ボラの順位で上位を持つ | 月初の営業日だけ | 終値 < SMA100・順位脱落・地合いオフ |
| `ross_cameron` | Gap & Go / マイクロプルバックの日足版 | 材料日とその直後の押し目 | 終値 < EMA9 |

```bash
wbjp screen   --config-dir config              # 順位表（--show-failed で落ちた理由）
wbjp backtest --config-dir config --from 2023-01-01
```

> 戦略は名前で呼び分ける。`config/<dir>/strategies.toml` の `name` を変えれば、既存の戦略を上書きせずに別の手法を試せる。使える戦略は `wbjp strategies` で一覧できる。

---

## 積立（`accum`）

スイング売買とは別プロジェクト。「今月いくら**追加**するか」を決めて買い増すだけで、売却も損切りもしない。設定は [`config/accum/accum.toml`](config/accum/accum.toml)、APIキーとデータ置き場は `wbjp` と共有する。

| | スイング売買（`wbjp`） | 積立（`accum`） |
|---|---|---|
| 戦略の出力 | 銘柄ごとに −1.0〜+1.0 の意見 | その日の購入倍率（1.0 以上） |
| 実行 | 合成 → サイジング → 差分発注 | 予算 × 倍率 → 成行買い |
| 設定 | `settings.toml` + `strategies.toml` の `[[strategies]]` | `accum.toml` の `[[tactics]]` / `[[baskets]]` |
| 例 | `sma_cross`, `trend_pullback`, `momentum_rank` | `bear_stack`, `stack_ladder`, `drawdown_ladder` |

```bash
accum strategies          # 使える戦略
accum list                # 戦略と銘柄の対応
accum sync                # 足を取る（日本株は J-Quants の配信範囲ぶん＝約10年。米国指数は約30年）
accum plan                # 直近の投下額
accum backtest            # 銘柄ごとの結果（対照群＝同額を均等に投じた場合）
accum compare 1305.T      # 登録済み戦略を1銘柄で横並び
accum run                 # 今日出すべき投下を計算する（注文は出さない）
accum run --live          # 注文を出す。口座は .env の WBJP_ENV（uat / prod）。cron では --yes も
```

倍率の判定を別の銘柄で行うこともできる（`signal_symbol`）。東証の S&P500 連動 ETF を買いながら、増額の判定は本家の指数 `^GSPC` の配列で行う、といった使い方。判定用は買わないので指数でもよく、暦が違っても「その日以前で最新」の判定値を当てる。判定用の市場の引けが買う市場の判断時刻より後（東証の銘柄を米国指数で判定）なら、同じ日付の足はまだ存在しないので**前日の足**を使う——バックテストでも同じ規則にして、ライブと食い違わないようにしている。省略すれば買う銘柄自身の足で判定する。

`run` は毎回 **「今月の目標（今日まで）− 今月の発注済み」** を銘柄ごとに計算し、差額があればその額を 1 件の成行買いにする。目標は入金日（月初の営業日）を過ぎていれば基本予算を含み、そこに今日までの増額分が積み上がる。発注済みは台帳（`state/accum-<env>.db`）から引く。この 1 本の規則で、月の途中から始めた場合（**開始月は残り暦日数で日割り**: 25,000 円で 9/16 開始なら 15/30 日ぶんの 12,500 円。開始日は銘柄ごとに最初の `--live` の日を台帳に記録し、翌月からは全額）、月の途中で予算を増やした場合（差額だけ）、cron が止まった日があった場合（次の実行で埋まる）、同じ日に 2 回走った場合（2 回目は 0）がすべて同じ扱いになる。ただし差額を**出す日**は増額と同じ規則に揃える: 直前の確定足が入金日か増額のリリース日（どのみち注文が出る日）か、差額が今月の基本目標以上（入金日の注文が通らなかった・cron が止まっていた・月の途中で始めた）のときだけ出し、それ以外の日は持ち越す。単元の端数や小さな予算増が、株価の下がった日に 1 単元だけの小口注文にならないようにするため。判断はバックテストと同じく前日までの確定足で行い、買うのは当日の価格。最終足が `max_stale_days`（既定 4 日）より古い銘柄は判定せず通知する。

増額分は日ごとに判定して積み上げ、**翌週の最初の営業日（月曜、休場なら火曜以降）に、累積が基本予算以上になっていれば 1 件でまとめて出す**（届かなければ次週へ持ち越して積み続け、入金日には閾値に関係なく基本分と同じ注文に乗せる）。日ごとに出すと 1 件が数千円になり、最低手数料 55 円で 1.5% 取られるうえ 10 口単位の ETF では 1 単元に届かないため。基本予算まで貯めてから出すことで 1 件あたりの手数料率を下げる。投下額の総量は変わらず、出す日が後ろにずれるだけ。単元に届かず買えなかった端数は差額として残り、次に資金が足される月曜か翌月の入金日に自動で繰り越される（台帳に残す「発注済み」は差額ではなく**株数 × 価格**）。月をまたいだ残りは、前月に実際の発注記録がある場合だけ当月の目標に足す——dry-run しかしていない月の分まで本稼働の初月に買わないため。

`run` は冒頭で、前回までに送った注文の**約定状況をブローカーに照会**して台帳を更新する。「発注済み」に数えるのは生きている注文と約定した分だけで、失効・拒否の未約定分は数えない——つまり次の `run` で差額として自動的に埋め直される。未約定のまま終わった注文は通知する。台帳は `accum orders`（`--check` で照会を伴う）で見られる。発注前にブローカーの見積りと買付余力を突き合わせ、足りなければ見送って通知する。株数は `投下額 ÷ 価格` を単元に切り捨て。ETF は単元が 1 株・10 株のものが多いので `[execution] lot_size_overrides` で指定する（既定の 100 株だと月の予算が届かず丸ごと見送りになる）。注文 ID は日付・銘柄・株数から決定論的に作るため、cron が二重に走っても二重買付にならない。

部品は `wbcore` と共有: 足は `wbcore.data`、発注は `wbcore.broker`、登録簿の仕組みは `wbcore.registry`。積立固有なのは倍率（`accum.tactics`）・計画（`accum.plan`）・検証（`accum.simulate` / `accum.basket`）・注文化（`accum.execute`）で、どの段も単独で差し替えられる。

> **初回の確認**: 最初は必ずデモ環境（`WBJP_ENV=uat`）の dry-run で `wbjp account` と `wbjp run` を通し、ログの発注電文を確認すること。

---

## 発注するかどうかは `--live` だけで決まる

| 軸 | 何を決めるか | 値 |
|---|---|---|
| **`--live`**（コマンド引数） | **注文を出すか** | 無し＝出さない（データ取得・判断・台帳への記録は行う）／有り＝出す |
| `WBJP_ENV`（`.env`） | **どの口座に繋ぐか** | `uat`＝テスト口座（実弾ではない）／`prod`＝本番口座 |
| `kill_switch`（設定ファイル） | 緊急停止 | `true` なら `--live` があっても出さない |

`WBJP_ENV` は売買の可否ではなく口座の選択。`wbjp run` / `accum run` は冒頭にこの 2 軸を 1 行で出す:

```
口座: 本番口座（WBJP_ENV=prod）  発注: しない（--live なし（データ取得と判断は行い、注文は出さない））
```

本番口座で `--live` を付けたときだけ、端末では続行の確認を求める（cron では `--yes` で省く）。

## 時刻の規約

| 場面 | 規則 |
|---|---|
| 保存 | 時刻は必ず時間帯付き。SQLite の `placed_at` は UTC の ISO 8601（`+00:00` 付き）。暦日（`date`）は取引所の日付で時刻ではないので時間帯を持たない |
| 演算・判定 | UTC。取引所の現地時刻が要る判断（発注時間帯・引けの前後）は、その場で `Market.timezone` に変換して比べる |
| 表示 | 設定の時間帯（`WBJP_TIMEZONE`、既定 UTC）。日本で運用するなら `.env` に `WBJP_TIMEZONE=Asia/Tokyo`。どの時間帯でも**略号を必ず添える**（`2026-08-29 06:20 UTC` / `15:20 JST`）。DB に UTC で保存された時刻（`placed_at` 等）も `explain` / `runs` では設定の時間帯に直して出す |
| ログ | 端末の表示は設定の時間帯（オフセット付き ISO）。ファイルのログには加えて `ts_utc`（常に UTC）が入る |

## ログ（後から AI に読ませる用）

**ファイルに残すログは 1 箇所だけ**——`WBJP_LOG_DIR`（既定 `WBJP_STATE_DIR/logs`＝`state/logs`）。機械が読む JSONL も、cron で stderr を残す場合もここに集める。

端末に出る整形表示とは別に、**機械が読む JSON Lines** を常に `<WBJP_LOG_DIR>/<app>-<env>.jsonl` に書く（1 行 1 レコード、日次ローテーション、90 日保持、秘匿情報は伏せる）。全レコードに `schema` / `ts_utc` / `run_id`（1 回の実行の識別子）/ `app` / `env` / `command` が付き、主要な出来事には安定した `code`（`accum.decision` / `accum.order` / `accum.fill` …）が付く。項目の定義は [docs/LOGGING.md](docs/LOGGING.md)。

```bash
jq -r 'select(.code == "accum.decision") | [.ts_utc, .symbol, .target, .placed, .due] | @tsv' state/logs/accum-prod.jsonl
```

「今」を取るのは `wbcore.clock` だけ（`now_utc()` / `today_utc()`）。`date.today()` や tz 無しの `datetime.now()` は cron のサーバーと開発機で結果が変わるので使わない——テストが監視している。tz 無しの datetime を受け取ったら UTC とみなす。

---

## 取引所（ブローカー）の差し替え

発注先は `wbcore.broker.base.Broker` 抽象クラスの裏に隠れている。売買（`wbjp`）も積立（`accum`）も `wbcore.broker.registry.connect(name, env, market=...)` を通るので、設定の1行で切り替わる。

```toml
[execution]
broker = "tachibana"   # tachibana | paper（ネットワークに繋がないシミュレータ）
```

取引所を足す手順:

1. `Broker` を継承し、`name`（設定で使う名前）と `connect()`（認証情報の解決・接続先の選択など、その証券会社固有の準備）を書く
2. `wbcore.broker.registry.BROKERS.register(YourBroker)` する

認証情報は証券会社ごとに名前空間と項目を分ける（立花証券は `TACHIBANA_<ENV>_AUTH_ID` / `_PRIVATE_KEY_FILE` / `_ORDER_PASSWORD`、キーチェーン `tachibana/<env>`）。解決の優先順位（環境変数 → `.env` → キーチェーン）は `wbcore.credentials._resolve_fields` が共通に持つ。

## 足データの取得元と足の間隔

取得元も同じ形で差し替える。`wbcore.data.provider.MarketDataProvider` 抽象クラスの裏に J-Quants / FRED が並び、設定の名前で選ぶ。省略すると市場の既定（日本株 `jquants` / 米国の指数 `fred`）。

```toml
[universe]
data_provider = "jquants"    # jquants（日本株・日足のみ）| fred（米国の指数・終値のみ）
```

扱うのは**日足だけ**。J-Quants も FRED も日足しか返さず、立花証券の API に足の履歴は無い。

取得元を足す手順はブローカーと同じ: `MarketDataProvider` を継承して `name` / `fetch_bars()` / `connect()` を書き、`wbcore.data.registry.PROVIDERS.register()` する。

---

## アーキテクチャ

戦略は「注文」ではなく「シグナル」を出す。注文への変換はエンジンが一手に引き受ける。

```
Strategy.on_bars(ctx) -> list[Signal]     戦略はI/Oを一切知らない
        ↓
SignalCombiner        複数戦略のシグナルを1本に合成
        ↓
PositionSizer         合成シグナル → 目標株数（単元株に丸め）
        ↓
Reconciler            目標 vs 実建玉＋未約定 の「差分だけ」を注文化
        ↓
RiskManager           上限チェック・拒否
        ↓
Broker                PaperBroker（証券会社の実装を足せる）
```

この構造が効くところ:

- **冪等性** — 「差分だけ発注」なので、クラッシュ後に再実行しても二重発注しない
- **テスト容易性** — 戦略は純粋な関数に近く、モック不要で単体テストできる
- **同じコードがバックテストでも本番でも動く** — `Broker` を差し替えるだけ

### 4 つの CLI が共有する入口（`wbcore/cli`）

`wbjp` / `accum` / `daytrade` / `jquants` の入口は `wbcore/cli` に集めてある——run_id の発行、ログ、
ダイジェスト、異常終了の通知（`Run.Crash`）、ブローカーへの接続（`ConnectBroker`）、本番発注前の
確認（`ConfirmLive`）、金額・日付の整形。コマンドごとに書くと「daytrade には通知があるが wbjp には無い」
のような抜けが起きるため。デイトレの発注（建玉・手仕舞い・照会の突き合わせ）は `daytrade/execute` に
あり、模型のブローカーと一時的な台帳で検証している（`cmd/` には判断の流れだけを残す）。

台帳（SQLite）のスキーマは `wbcore/storage.Migrate` で版管理する（`PRAGMA user_version`）。
列を足すときは各台帳の `migrations` の末尾に段を足す。

---

## 技術スタック

| 領域 | 選定 | 理由 |
|---|---|---|
| 言語 | Go 1.26 | 単一の実行ファイルで配れる。cron から叩くのにランタイムの用意が要らない |
| パッケージ管理 | Go modules | `go.sum` で再現性。追加のツールは要らない |
| 数値 | shopspring/decimal | 金額・株数は二進浮動小数点に載せない（丸め誤差が注文数量に出る） |
| 価格データ | J-Quants API（日本株）/ FRED（米国の指数） | 日本株は JPX 公式・調整済み四本値。米国の指数は判断材料にだけ使うので、無料・キー不要・pandas 不要の FRED。取得済みは必ずローカルキャッシュ |
| 状態の保存 | SQLite | 注文・シグナル・実行履歴。ACID と冪等性の担保 |
| 時系列の保存 | Parquet + DuckDB | 足データの高速な集計・分析 |

---

## セットアップ

```bash
deploy/build.sh              # bin/ に wbjp / accum / daytrade / jquants / discord-post を作る
export PATH="$PWD/bin:$PATH"
```

必要なのは Go 1.26 以上だけ。`go build ./...` でも同じものが作れる。
DuckDB を使う機能（`jquants query`）は cgo を要求するので、C コンパイラが要る。

---

## 安全装置

実発注には**2つの条件が同時に**必要。どちらか欠けたら dry-run になる。

1. `WBJP_ENV=prod`
2. CLI に `--live` フラグ

さらに `config/settings.toml` の `risk.kill_switch = true` で全発注を即停止できる。

APIキーはリポジトリに置かない。
（SDK はエラー時にリクエストヘッダ全体をログ出力するため、ログには秘匿情報マスクを必ず通す。）

---

## APIキーの置き場所

3つのソースを上から順に見て、**項目ごとに**最初に見つかった値を使う。

| 優先 | ソース | 用途 |
|---|---|---|
| 1 | 環境変数 | systemd の `EnvironmentFile=` / CI（サーバー運用の推奨） |
| 2 | `.env` | キーチェーンの無いホスト。**`chmod 600` 必須** |
| 3 | OS キーチェーン | ローカル開発の推奨 |

変数名は `TACHIBANA_<ENV>_AUTH_ID` / `_PRIVATE_KEY_FILE` / `_ORDER_PASSWORD`
（例: `TACHIBANA_PROD_AUTH_ID`）。J-Quants の API キーは環境で分けず `WBJP_JQUANTS_API_KEY`。
どこから読まれているかは `wbjp credentials check --env prod` の「取得元」で確認できる。

### ローカル開発（macOS など）

キーチェーンへの**書き込みは OS の道具で行う**（このリポジトリに保存用の
コマンドは無い。秘密を書く経路を増やさないため）。サービス名は
`<namespace>/<env>`（小文字）、キーは環境変数名と同じ。

```bash
security add-generic-password -s tachibana/uat -a TACHIBANA_UAT_AUTH_ID -w
wbjp credentials check --env uat   # 取得元が「キーチェーン」になれば成功
```

### Linux サーバー

ヘッドレスな Linux には SecretService も D-Bus も無く keyring が使えないため、
秘密は systemd 経由で環境変数として渡すのが堅い。

```bash
sudo install -d -m 750 -o root -g wbjp /etc/wbjp
sudo install -m 640 -o root -g wbjp /dev/null /etc/wbjp/wbjp.env
sudo vi /etc/wbjp/wbjp.env   # TACHIBANA_PROD_AUTH_ID=... 等（.env.example 参照）
```

```ini
# /etc/systemd/system/wbjp.service
[Service]
User=wbjp
EnvironmentFile=/etc/wbjp/wbjp.env
WorkingDirectory=/opt/wbjp
ExecStart=/opt/wbjp/bin/wbjp run --live
```

リポジトリ内の `.env` は `WBJP_ENV` やログ設定など秘密でない項目に使う。
`.env` に秘密を書く場合は `chmod 600` すること（緩ければ起動時に警告が出る）。

`.env` は読み取って内部の設定に持つだけで、プロセスの環境変数には**入れない**。
子プロセスや `/proc/<pid>/environ` から秘密が読めないようにするため。

---

## 使い方

```bash
# UAT（テスト口座。実弾なし）
WBJP_ENV=uat wbjp account
WBJP_ENV=uat wbjp run --live

# バックテスト
wbjp backtest --from 2023-01-01 --to 2026-06-30
# 指値の約定モデルを変えて突き合わせる。既定の open は寄付だけで判定（保守的）、
# intrabar はその足の高安に届けば約定（楽観的）。判断ロジックは同一なので、
# 成行だけの設定なら両者は完全に一致する。差は指値の約定しやすさの違いだけ
wbjp backtest --from 2023-01-01 --to 2026-06-30 --fill-model intrabar

# 本番（--live なしは常に dry-run）
WBJP_ENV=prod wbjp run
WBJP_ENV=prod wbjp run --live
```

---

## J-Quants データの蓄積（`jquants`）

日本株の四本値だけでなく、財務・決算予定・投資部門別・信用残・空売り・指数・EDINET など Standard プランで取れる**全端点**をローカルに溜める（設計は [docs/JQUANTS_ARCHIVE.md](docs/JQUANTS_ARCHIVE.md)）。API は 10 年しか遡れないので、溜め始めた日から手元の履歴が伸びる。

```bash
# 初回: 一括ダウンロード（月次 csv.gz）で全期間（約 15 分）
jquants backfill
# 一括に無い端点（EDINET 3 種・決算予定）を日付で遡る（約 75 分。夜に）
jquants sync --days 3650
# 日次: 台帳を見て必要な分だけ（cron で固定間隔）
jquants sync
# 端点ごとの保存状況 / 直近 30 日の営業日に欠けが無いか（あれば非 0）
jquants status
jquants check
jquants query "SELECT Code, DiscDate, NP FROM fins_summary WHERE Code='72030' ORDER BY DiscDate DESC LIMIT 4"
```

（zsh の対話シェルは行の途中の `#` をコメントとして扱わないので、コマンドの後ろにコメントを付けたまま貼らないこと）

置き場は `data/jquants/<端点>/<YYYY-MM>.parquet`（生のまま・全列文字列・鍵で後勝ち）と台帳 `data/jquants/ledger.db`。`data/` は再取得できるキャッシュなのでホスト間でコピーしてよい（発注台帳やログは `state/` にあり、こちらは上書き厳禁）。`accum sync` の日本株の足もここを経由する（揃っていれば API を叩かない）。

## 戦略の改善ループ（本番と同じデータで回す）

cron が溜め続ける J-Quants アーカイブ（`data/jquants`、2016 年〜）と FRED の指数を、そのまま研究にも使う。
判断ロジックはライブと同じコードなので、ここで出した結果がそのまま運用の見立てになる。

| 段階 | コマンド | 何が出るか |
|---|---|---|
| データ | `jquants status` / `jquants check` | 端点ごとの蓄積状況と営業日の欠け |
| | `jquants sync` | 足りない日付だけ取る（cron が 30 分ごとに回している） |
| デイトレ | `daytrade plan --config-dir config/daytrade_margin` | 翌営業日の母集団（`state/daytrade/plan-<日付>.parquet`）。cron が 20:30 に回す |
| | `daytrade backtest --config-dir config/daytrade_margin --since 2025-01-01` | ロング＋ショートの年別損益・Sharpe・最大 DD（数秒） |
| スイング | `wbjp data sync --config-dir config` | `universe.symbols` の日足をアーカイブから揃える |
| | `wbjp screen --config-dir config` | 戦略の合致度の順位表（`--show-failed` で落ちた理由） |
| | `wbjp backtest --config-dir config --from 2025-01-01` | 資産曲線・勝率・シャープ。`--fill-model intrabar` で約定モデルを変えて突き合わせ |
| 積立 | `accum sync` → `accum backtest` / `accum compare 1306.T` | 戦略ごとの結果と対照群（S&P500 等の判定用指数は FRED） |

### 運用の結果から改善する（[docs/FEEDBACK.md](docs/FEEDBACK.md)）

バックテストは規則を過去に当て直すが、**実際に運用が見た値**からも改善の材料を取る。

| 段階 | コマンド | 何が出るか |
|---|---|---|
| 今日どう動いたか | `jq 'select(.anomalies)' state/digest/prod-<日付>.jsonl` | 実行 1 回が 1 行。異常のあったものだけ絞れる |
| 選定は効いたか | `daytrade review` / `wbjp review` / `accum evaluate` | 選んだものと選ばなかったものの比較 |
| 約定は想定どおりか | `state/<app>/history/execution/*.parquet` | 想定価格との差（bp）、見送りの理由の分布 |

`review` / `evaluate` / `history` / `screen` には `--json` があり、表の代わりに JSON を
1 個だけ出す（`jq` にそのまま渡せる）。

パラメータは `config/<dir>/*.toml` を書き換えて再実行する。別案を試すときは `config/` をディレクトリごと複製し
`--config-dir` で指す（cron が読む設定を壊さない）。研究の記録は `docs/research/` に日付付きで残す。

## cron で回す

`wbjp run` は**1サイクルだけ**実行して終了する。定期実行はここに書く cron が担当する。

日足で判断するため、走らせるのは**大引け後に1日1回**。J-Quants の当日足が
入るのを待つので、配信後（Light プランは翌朝）に回す。

```cron
# 平日 16:30 に日次サイクルを実行
CRON_TZ=Asia/Tokyo
# stderr も JSONL と同じ WBJP_LOG_DIR に残す（ログの置き場は 1 箇所）。systemd なら journald でよい
30 16 * * 1-5 cd /opt/wbjp && WBJP_ENV=prod /opt/wbjp/bin/wbjp run --live --yes >> state/logs/wbjp-run.log 2>&1
```

cron で詰まりやすい点:

- **`--yes` が要る。** 本番の `--live` は確認プロンプトを出すが、cron には
  stdin が無い。`--yes` が無いと理由をログに残して exit 1 で止まる
  （黙って Abort はしない）
- **`cd` が要る。** `config/` `data/` `.env` はカレントディレクトリ基準。
  cron は `$HOME` で起動するので、`cd` しないと設定が見つからない。
  `cd` を使わないなら `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` で `.env` を
  絶対パス指定できる
- **フルパスで呼ぶ。** cron の `PATH` は最小限。`bin/wbjp` を直接叩く
- **祝日は考慮されない。** 東証の休場日にも起動するが、新しい足が増えないため
  基準日が前営業日のままになり、同じ注文IDが再生成されて冪等に弾かれる
- **同じ日の二重実行は安全。** 注文IDは「取引日 × 銘柄 × 売買 × 数量」から
  決定論的に作られるので、cron が二重に走っても、失敗後に手で再実行しても、
  同じ注文が2回出ることはない

止めたいときは `config/settings.toml` の `risk.kill_switch = true`。
cron を消さなくても次のサイクルから発注しなくなる。

---

## 構成

```
pkg/wbcore/              共通基盤（wbjp / accum / daytrade のどれからも使う。逆向きの依存は無い）
├── credentials/         認証情報の解決（キーチェーン / 環境変数 / .env。秘密はここを通す）
├── settings/            環境変数由来の設定（WBJP_ENV など）と実発注の可否判定
├── registry/            名前 → 実装 の登録簿（戦略・取得元の登録に使う）
├── logging/             構造化ログ（JSONL）+ 秘匿情報マスク
├── domain/models.go     Bar / Signal / Order / Position（すべて Decimal）、決定論的な注文ID
├── marketrules/         取引ルール（呼値・値幅制限・単元株・取引時間・差金決済。実装は東証のみ）
├── data/                MarketDataProvider → J-Quants / FRED / CSV、BarStore（Parquet）
├── indicators/          SMA/EMA/RSI/ATR/MACD/BB/ADX/Donchian
├── history/             追記専用の Parquet 履歴と DuckDB での読み出し
├── execution/           実行品質（判断した値と約定した値の差）の記録
├── notify/              Webhook 通知（アラート / 日次レポートの配達）
└── broker/              Broker → 立花証券 e支店 API / Paper / レート制限

pkg/wbjp/                スイング売買
├── config/              settings.toml / strategies.toml（ユニバース・リスク・出口・レジーム）
├── strategy/            Strategy / 合成器4種 / サンプル戦略7本
├── portfolio/sizer.go   equal_weight / fixed_notional / atr_risk
├── risk/                リスク上限 / ストップの合成 / レジーム
├── engine/              リコンサイル / バックテスト / 成績分析
├── repo/                SQLite の判断ジャーナル
├── evaluate/            事後検証（採用・次点・圏外の比較）
└── history/

pkg/accum/               積立
├── config/              accum.toml（戦略と銘柄の対応・予算・発注）
├── tactics/             Tactic / constant / bear_stack / stack_ladder / drawdown_ladder
├── window/              発注時間帯
├── plan/                日足と戦略 → 日ごとの投下額
├── simulate/, basket/   検証（対照群との比較 / 複数銘柄への配分）
├── execute/             投下額 → 買い注文、前回注文の照会（sync.go）
└── ledger/              SQLite の台帳（当月いくら発注済みか）

pkg/daytrade/            デイトレ（ギャップ逆張り）
├── config/, calendar/   設定と取引カレンダー
├── universe/, selection/ 母集団の構築と当日の絞り込み
├── regime/, usmarket/   地合いの判定（IV・TOPIX ドリフト・米国市場）
├── plan/, quotes/       候補の作成と気配の取得
├── backtest/            資金固定・単元・段階手数料つきの検証
├── evaluate/            事後検証と `review`
└── ledger/, fees/, history/

pkg/jquants/archive/     J-Quants の生データ保管庫（端点ごとの月次 Parquet + 取り込み台帳）

cmd/wbjp, cmd/accum, cmd/daytrade, cmd/jquants   各 CLI（cobra）
cmd/discord-post                                  日次レポートの配達係
```

### 実装上の注意点（実機で確認した挙動）

| 箇所 | 内容 |
|---|---|
| `logging/` | 秘密は `RegisterSecret` を通したものだけがマスクされる。認証情報を増やしたら登録も一緒に書く |
| `marketrules/` | 呼値は「以下」区分、値幅制限は「未満」区分。引き方が違う |
| `indicators/` | Wilder 平滑化の種は先頭の差分を落とさずに作る。0 で埋めると TA-Lib と値がずれる |
| `wbjp/engine/backtest.go` | 指標は全期間で一度だけ計算する（指標が因果的なので等価）。毎日再計算すると日数の二乗で遅くなる |
| `broker/paper.go` | 注文の失効は**約定処理のあと**。先に失効させると注文が一度も約定しない |
| `wbjp/engine/analysis.go` | 決済トレードは約定列を銘柄ごと FIFO で突き合わせて作る。シャープは 1 本ごとのリターンの標本標準偏差で年率換算（245 営業日）。値が出せないときは "-" |
| `data/jquants_code.go` | 株式は**調整済み**（`AdjO`/`AdjC` 等）を読む。未調整のままだと分割日に巨大な偽の下落が現れる |
| `accum/execute/sync.go` | 応答が返らず PENDING のまま残った注文は 1 日後に REJECTED に落とす。放置すると永久に「発注済み」として当月の予算を食う |

## 開発

```bash
go test ./...              # ネットワークを使うものは既定でスキップ
go test ./... -short       # 時間のかかる検証を飛ばす
go vet ./...
make ci                    # build + vet + staticcheck + test（GitHub Actions と同じ手順）
gofmt -l .                 # 整形されていないファイルを列挙
```

## 次にやること

1. `config/settings.toml` の `universe.symbols` を実際に売買したい銘柄に変える
   （TOPIX500 構成銘柄は `topix500_symbols` にも入れる。呼値が変わる）
2. `risk.max_order_value_jpy` を自分の資金規模に合わせる（既定の50万円は保守的）
3. 立花証券のデモ環境の認証情報を設定し（docs/DEPLOY.md「立花証券 e支店」）、
   `WBJP_ENV=uat wbjp run --live` を数日回して `wbjp explain <run_id>` で判断を目視検証
4. 本番の認証ID・鍵を設定 → `wbjp credentials check --env prod`
5. `WBJP_ENV=prod wbjp run`（dry-run）を数日回してから `--live` に進む

