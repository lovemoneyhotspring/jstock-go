# wbjp — Webull証券 日本株・米国株 自動売買システム

Webull証券の OpenAPI を使って日本株・米国株を売買するシステム。3つのパッケージからなる。

| パッケージ | 役割 | CLI |
|---|---|---|
| `wbcore` | 共通基盤。ブローカー・足データ・指標・認証情報・登録簿の仕組み | — |
| `wbjp` | スイング売買。複数の戦略を差し替え・合成し、差分だけを発注する | `uv run wbjp` |
| `accum` | 積立。ドル平均法＋下落局面での増額。売らない | `uv run accum` |

`wbjp` と `accum` は互いに import しない。どちらも `wbcore` の部品を組み合わせて動く。

> **⚠️ 免責**
> 本ソフトウェアは投資助言ではありません。自動売買は資産を失う可能性があります。
> 必ず UAT（テスト）環境で十分に検証し、自己責任で利用してください。

---

## 設計の前提（実機調査で確定）

Webull JP OpenAPI には、設計を左右する2つの制約がある。

### 1. 日本株の株価データは Webull API から取得できない

Webull の市場データAPI（`bars` / `snapshot`）は `US_STOCK` / `US_ETF` にしか対応していない。
日本株は **発注はできるが、足データは取れない**。

→ **価格データ層とブローカー層を完全に分離**し、価格は yfinance（`7203.T` 形式）から取得する。

### 2. 日本株は成行・指値のみ。逆指値は使えない

| 注文種別 | 米国株 | 日本株 |
|---|:---:|:---:|
| MARKET（成行） | ✓ | ✓ |
| LIMIT（指値） | ✓ | ✓ |
| STOP_LOSS（逆指値） | ✓ | **✗** |
| STOP_LOSS_LIMIT | ✓ | **✗** |

→ **損切りはエンジン側で合成する**（`wbjp.risk.stops`）。
ストップ価格をローカルに保持し、日足更新時に評価して成行/指値の決済注文を出す。

> **重要な限界**: 日足運用のため、場中の急落にはその日のうちに反応できない。
> 想定より大きな損失が出る可能性がある。この制約を理解した上で使うこと。

---

## 米国株

市場ごとの制約は `wbcore.domain.market_rules` に集約してあり、設定の `universe.market` を切り替えるだけでエンジン本体は同じコードで動く。米国株の設定例は [`config/us/`](config/us/) にある。

```bash
uv run wbjp data sync --config-dir config/us
uv run wbjp backtest --from 2024-01-01 --config-dir config/us
uv run wbjp run --config-dir config/us            # 判断まで（注文は出さない）
```

| | 日本株（`market = "JP"`） | 米国株（`market = "US"`） |
|---|---|---|
| 売買単位 | 100株（単元） | 1株 |
| 呼値 | 価格帯で段階（`jp_rules`） | 0.01ドル固定 |
| 制限値幅 | あり（指値が弾かれる） | なし |
| 逆指値 | API 非対応 → エンジン合成 | **STOP_LOSS を GTC でブローカーに置く** |
| 差金決済の回避 | 当日買った銘柄は当日売らない | 不要 |
| 通貨 | JPY | USD（`risk` の金額もドル建て） |
| 足データ | yfinance | yfinance か Webull 市場データ API |
| 売買手数料 | 約 0.11%（UAT 実測） | 無料（2026-07-27〜）＋ SEC/FINRA 手数料 |

米国株で逆指値をブローカーに置く場合の要点:

- ストップ価格の記録（`stops` テーブル）は引き続きエンジンが持つ。毎サイクル、記録と板上の逆指値を突き合わせて差分だけ置き直す（`wbjp.risk.stops.sync_broker_stops`）。冪等なので再実行しても増えない。
- **逆指値は実効ポジションに数えない。** 数えると「もうすぐ建玉が消える」と誤認して買い直す。`effective_quantity` が除外する。
- 戦略が手仕舞いを決めた銘柄は、売り注文を出す前に逆指値を取り消す。両方が板に乗ると二重売却になる。
- 日足のストップ判定も保険として残す。逆指値が何かの理由で消えていた日でも翌寄付で手仕舞える。
- `execution.stop_mode = "engine"` にすれば米国株でも日足判定だけに戻せる（比較検証用）。

同じ口座に日本株と米国株が混在するため、`WebullBroker` は **設定された市場の通貨の行だけ**を残高・建玉として読む。日本株と米国株を両方回すなら、設定ディレクトリを分けて別プロセスで動かす。

### スクリーニングと順位付け（`trend_pullback`）

米国株の設定は `trend_pullback` 戦略が「スクリーニング＋順位付け」を兼ねる。`config/us/universe.txt` の銘柄（＝allowlist）を毎サイクル全件評価し、条件を満たした銘柄にスコア（`direction` 0.3〜1.0）を付ける。サイジングは direction の高い順に `sizing.max_positions` の枠を埋めるので、**スコアがそのまま採用順位**になる。

```bash
uv run wbjp screen --config-dir config/us                 # 順位表
uv run wbjp screen --config-dir config/us --show-failed   # 落ちた銘柄と理由
```

| 条件 | 意図 |
|---|---|
| 終値 > SMA200 かつ SMA50 > SMA200、SMA50 が上向き | 長期上昇トレンドのみ |
| 60日高値からの下落 ≦ 15% | 押し目であって崩れではない |
| 前日の RSI(3) < 20 ＋ 当日が陽線 | 売られすぎ ＋ 反転確認（落ちるナイフを受けない） |
| ATR/終値 1.5〜5%、20日平均売買代金 ≧ 500万ドル | 動く・滑らない銘柄 |
| SPY > SMA50 | 地合いフィルタ |
| 決算 3 営業日前〜当日は新規建てせず、保有中なら手仕舞い | 日足ではギャップを避けられない（`config/us/earnings.toml` を手動更新） |

スコア = 0.35×押し目の深さ（RSI） + 0.30×SMA20 からの乖離（ATR 単位） + 0.20×トレンドの強さ + 0.15×流動性。内訳は `signals.meta_json` に残る。

手仕舞いは3層: 戦略（RSI(3) ≧ 80 / 含み益で SMA20 回復 / 60日高値到達）、`[stops]`（+1R で建値移動 → ATR トレーリング、5営業日で含み益なし、最大10営業日）、そして初期ストップ（1.5×ATR、米国株は逆指値として板に置く）。

### モメンタム順位戦略（`momentum_rank`、`config/us-momentum/`）

押し目買いとは別の、**損益比型**の戦略。過去 6 ヶ月（直近 1 ヶ月を除く）のリターンをボラティリティで割った順位で上位 `top_n` 銘柄を持ち、月初にだけ入れ替える。数十年・複数市場で生き残っているモメンタム効果に、地合いフィルタ（SPY > SMA200 でなければ全建玉手仕舞い）を重ねてモメンタムクラッシュを避ける。

```bash
uv run wbjp data sync --config-dir config/us-momentum --days 1500   # 12ヶ月の履歴＋ウォームアップが要る
uv run wbjp screen   --config-dir config/us-momentum
uv run wbjp backtest --config-dir config/us-momentum --from 2023-09-01
```

| | 押し目買い（`config/us`） | モメンタム（`config/us-momentum`） |
|---|---|---|
| 狙い | 勝率（小さく多く） | 損益比（大きく少なく） |
| 建て | 条件成立日に毎日 | 月初の営業日だけ |
| 降り | RSI 過熱・SMA20 回復・時間切れ | 終値 < SMA100・順位脱落・地合いオフ |
| 損切り | 1.5×ATR（＋建値移動・利確） | 3×ATR トレーリングのみ |
| 想定勝率 / 損益比 | 60% / 1:1 | 45% / 2.5:1 |

> 押し目買いの検証では、戦略の手仕舞い自体は勝率 90% で機能した一方、タイトな損切りと時間切れが利益を刈っていた（出口の非対称性）。モメンタムはその逆で、出口を広く取って勝ちを伸ばす。

### ロス・キャメロン流モメンタム（`ross_cameron`、`config/us-cameron/`）

Warrior Trading のロス・キャメロンの手法（Gap & Go / マイクロプルバック、9EMA トレーリング、損益比 2:1）を**日足のスイングに翻訳**したもの。本家は分足のデイトレードなので、翻訳の対応表と条件の一覧は [ross_cameron.py](src/wbjp/strategy/samples/ross_cameron.py) の冒頭にまとめてある。

```bash
uv run wbjp data sync --config-dir config/us-cameron --days 400
uv run wbjp screen   --config-dir config/us-cameron
uv run wbjp backtest --config-dir config/us-cameron --from 2019-01-01
```

| 入口 | 条件 |
|---|---|
| Gap & Go（材料日に乗る） | 始値が前日終値比 ≧ 3%、RVOL ≧ 2x、終値がレンジ上位 30% の陽線、直近 20 日高値を上抜け、終値 > EMA9 > EMA20 |
| マイクロプルバック（材料日のあと） | 直近 5 日以内に材料日 → 1〜3 日の押し目（安値が EMA9 の上）→ 前日高値を出来高伴って陽線で抜ける |

スコア = 0.35×RVOL + 0.30×ギャップ + 0.20×引け強度 + 0.15×ブレイク幅（ATR 単位）。出口は戦略側の終値 < EMA9 と、`[stops]` の -5% 初期ストップ／+1R 建値移動／+2R 半分利確／3 営業日で含み益なし。決算は本家にとって「材料」なのでブラックアウトは既定で無効。

> 戦略は名前で呼び分ける。`config/<dir>/strategies.toml` の `name` を変えれば、既存の戦略を上書きせずに別の手法を試せる。使える戦略は `uv run wbjp strategies` で一覧できる。

---

## 積立（`accum`）

スイング売買とは別プロジェクト。「今月いくら**追加**するか」を決めて買い増すだけで、売却も損切りもしない。設定は [`config/accum/accum.toml`](config/accum/accum.toml)、APIキーとデータ置き場は `wbjp` と共有する。

| | スイング売買（`wbjp`） | 積立（`accum`） |
|---|---|---|
| 戦略の出力 | 銘柄ごとに −1.0〜+1.0 の意見 | その日の購入倍率（1.0 以上） |
| 実行 | 合成 → サイジング → 差分発注 | 予算 × 倍率 → 成行買い |
| 設定 | `settings.toml` + `strategies.toml` の `[[strategies]]` | `accum.toml` の `[[tactics]]` / `[[baskets]]` |
| 例 | `trend_pullback`, `ross_cameron`, `momentum_rank` | `bear_stack`, `stack_ladder`, `drawdown_ladder` |

```bash
uv run accum strategies          # 使える戦略
uv run accum list                # 戦略と銘柄の対応
uv run accum sync                # 足を取る（約30年ぶん。増額は暴落局面で効くため長い履歴が要る）
uv run accum plan                # 直近の投下額
uv run accum backtest            # 銘柄ごとの結果（対照群＝同額を均等に投じた場合）
uv run accum compare 1305.T      # 登録済み戦略を1銘柄で横並び
uv run accum run                 # 今日出すべき投下を計算する（注文は出さない）
uv run accum run --live          # 注文を出す。口座は .env の WBJP_ENV（uat / prod）。cron では --yes も
```

倍率の判定を別の銘柄で行うこともできる（`signal_symbol`）。東証の S&P500 連動 ETF を買いながら、増額の判定は本家の指数 `^GSPC` の配列で行う、といった使い方。判定用は買わないので指数でもよく、暦が違っても「その日以前で最新」の判定値を当てる。判定用の市場の引けが買う市場の判断時刻より後（東証の銘柄を米国指数で判定）なら、同じ日付の足はまだ存在しないので**前日の足**を使う——バックテストでも同じ規則にして、ライブと食い違わないようにしている。省略すれば買う銘柄自身の足で判定する。

`run` は毎回 **「今月の目標（今日まで）− 今月の発注済み」** を銘柄ごとに計算し、差額があればその額を 1 件の成行買いにする。目標は入金日（月初の営業日）を過ぎていれば基本予算を含み、そこに今日までの増額分が積み上がる。発注済みは台帳（`data/accum-<env>.db`）から引く。この 1 本の規則で、月の途中から始めた場合（**開始月は残り暦日数で日割り**: 25,000 円で 9/16 開始なら 15/30 日ぶんの 12,500 円。開始日は銘柄ごとに最初の `--live` の日を台帳に記録し、翌月からは全額）、月の途中で予算を増やした場合（差額だけ）、cron が止まった日があった場合（次の実行で埋まる）、同じ日に 2 回走った場合（2 回目は 0）がすべて同じ扱いになる。判断はバックテストと同じく前日までの確定足で行い、買うのは当日の価格。最終足が `max_stale_days`（既定 4 日）より古い銘柄は判定せず通知する。

増額分は日ごとに判定して積み上げ、**翌週の最初の営業日（月曜、休場なら火曜以降）に 1 件でまとめて出す**。日ごとに出すと 1 件が数千円になり、最低手数料 55 円で 1.5% 取られるうえ 10 口単位の ETF では 1 単元に届かないため。単元に届かず買えなかった端数は差額として残り、次に資金が足される月曜か翌月の入金日に自動で繰り越される（台帳に残す「発注済み」は差額ではなく**株数 × 価格**）。月をまたいだ残りは、前月に実際の発注記録がある場合だけ当月の目標に足す——dry-run しかしていない月の分まで本稼働の初月に買わないため。

`run` は冒頭で、前回までに送った注文の**約定状況をブローカーに照会**して台帳を更新する。「発注済み」に数えるのは生きている注文と約定した分だけで、失効・拒否の未約定分は数えない——つまり次の `run` で差額として自動的に埋め直される。未約定のまま終わった注文は通知する。台帳は `uv run accum orders`（`--check` で照会を伴う）で見られる。発注前にブローカーの見積りと買付余力を突き合わせ、足りなければ見送って通知する。株数は `投下額 ÷ 価格` を単元に切り捨て。ETF は単元が 1 株・10 株のものが多いので `[execution] lot_size_overrides` で指定する（既定の 100 株だと月の予算が届かず丸ごと見送りになる）。注文 ID は日付・銘柄・株数から決定論的に作るため、cron が二重に走っても二重買付にならない。

部品は `wbcore` と共有: 足は `wbcore.data`、発注は `wbcore.broker`、登録簿の仕組みは `wbcore.registry`。積立固有なのは倍率（`accum.tactics`）・計画（`accum.plan`）・検証（`accum.simulate` / `accum.basket`）・注文化（`accum.execute`）で、どの段も単独で差し替えられる。

> **UAT で未確認の点**: 米国株の発注ペイロード（`trade_currency` / `extended_hours_trading` / `support_trading_session` の値）と、Webull 市場データ API の応答形式は SDK と公開ドキュメントから組んであり、実機での疎通確認がまだ。最初は必ず `WBJP_ENV=uat` の dry-run で `wbjp account` と `wbjp run` を通し、ログの発注ペイロードを確認すること。

---

## 発注するかどうかは `--live` だけで決まる

| 軸 | 何を決めるか | 値 |
|---|---|---|
| **`--live`**（コマンド引数） | **注文を出すか** | 無し＝出さない（データ取得・判断・台帳への記録は行う）／有り＝出す |
| `WBJP_ENV`（`.env`） | **どの口座に繋ぐか** | `uat`＝Webull のテスト口座（実弾ではない）／`prod`＝本番口座 |
| `kill_switch`（設定ファイル） | 緊急停止 | `true` なら `--live` があっても出さない |

`WBJP_ENV` は売買の可否ではなく口座の選択。`wbjp run` / `accum run` は冒頭にこの 2 軸を 1 行で出す:

```
口座: 本番口座（WBJP_ENV=prod）  発注: しない（--live なし（データ取得と判断は行い、注文は出さない））
```

本番口座で `--live` を付けたときだけ、端末では続行の確認を求める（cron では `--yes` で省く）。

## 時刻の規約

| 場面 | 規則 |
|---|---|
| 保存 | 時刻は必ず時間帯付き。日中足の `ts` は UTC（Parquet に時間帯が残る）、SQLite の `placed_at` は UTC の ISO 8601（`+00:00` 付き）。暦日（`date`）は取引所の日付で時刻ではないので時間帯を持たない |
| 演算・判定 | UTC。取引所の現地時刻が要る判断（発注時間帯・引けの前後・分足の区切り）は、その場で `Market.timezone` に変換して比べる |
| 表示 | 設定の時間帯（`WBJP_TIMEZONE`、既定 UTC）。日本で運用するなら `.env` に `WBJP_TIMEZONE=Asia/Tokyo`。どの時間帯でも**略号を必ず添える**（`2026-08-29 06:20 UTC` / `15:20 JST`）。DB に UTC で保存された時刻（`placed_at` 等）も `explain` / `runs` では設定の時間帯に直して出す |
| ログ | 端末の表示は設定の時間帯（オフセット付き ISO）。ファイルのログには加えて `ts_utc`（常に UTC）が入る |

## ログ（後から AI に読ませる用）

**ファイルに残すログは 1 箇所だけ**——`WBJP_LOG_DIR`（既定 `WBJP_DATA_DIR/logs`＝`data/logs`）。機械が読む JSONL も、cron で stderr を残す場合もここに集める。SDK が勝手に作る `webull_*_sdk.log` は抑止してあり、どこにも書かれない。

端末に出る整形表示とは別に、**機械が読む JSON Lines** を常に `<WBJP_LOG_DIR>/<app>-<env>.jsonl` に書く（1 行 1 レコード、日次ローテーション、90 日保持、秘匿情報は伏せる）。全レコードに `schema` / `ts_utc` / `run_id`（1 回の実行の識別子）/ `app` / `env` / `command` が付き、主要な出来事には安定した `code`（`accum.decision` / `accum.order` / `accum.fill` …）が付く。項目の定義は [docs/LOGGING.md](docs/LOGGING.md)。

```bash
jq -r 'select(.code == "accum.decision") | [.ts_utc, .symbol, .target, .placed, .due] | @tsv' data/logs/accum-prod.jsonl
```

「今」を取るのは `wbcore.clock` だけ（`now_utc()` / `today_utc()`）。`date.today()` や tz 無しの `datetime.now()` は cron のサーバーと開発機で結果が変わるので使わない——テストが監視している。tz 無しの datetime を受け取ったら UTC とみなす。

---

## 取引所（ブローカー）の差し替え

発注先は `wbcore.broker.base.Broker` 抽象クラスの裏に隠れている。売買（`wbjp`）も積立（`accum`）も `wbcore.broker.registry.connect(name, env, market=...)` を通るので、設定の1行で切り替わる。

```toml
[execution]
broker = "webull"   # webull | paper（ネットワークに繋がないシミュレータ）
```

取引所を足す手順:

1. `Broker` を継承し、`name`（設定で使う名前）と `connect()`（認証情報の解決・接続先の選択など、その証券会社固有の準備）を書く
2. `wbcore.broker.registry.BROKERS.register(YourBroker)` する

認証情報は証券会社ごとに名前空間を分ける（`load_credentials(env, namespace="XXX")` → `XXX_PROD_APP_KEY` / キーチェーン `xxx/prod`）。Webull は `WBJP`。CLI には手を入れない。

## 足データの取得元と足の間隔

取得元も同じ形で差し替える。`wbcore.data.provider.MarketDataProvider` 抽象クラスの裏に yfinance / Webull 市場データ API が並び、設定の名前で選ぶ。

```toml
[universe]
data_provider = "yfinance"   # yfinance（両市場）| webull（米国株のみ）
```

足の間隔（`Interval`: `1d` / `1h` / `30m` / `15m` / `5m` / `1m`）は抽象の一部で、取得元ごとに対応範囲を申告する（yfinance の 1 分足は直近 7 日、5〜30 分足は 60 日まで）。日中足は UTC の `ts` と暦日 `date` の両方を持ち、`data/bars/<間隔>/` に日足とは別に保存される。

```bash
uv run wbjp data sync --interval 5m --days 5    # 5分足を取る（data/bars/5m/）
```

取得元を足す手順はブローカーと同じ: `MarketDataProvider` を継承して `name` / `intervals` / `fetch_bars()` / `connect()` を書き、`wbcore.data.registry.PROVIDERS.register()` する。

### 細かい足を基準に取り込み、粗い足は合成する

```toml
[universe]
interval = "5m"        # 判断に使う足
base_interval = "1m"   # 取り込みの基準。5分足・1時間足・日足はここから合成する
```

`base_interval` を設定すると、`data sync` は基準足と `interval` の足を両方取り、`backtest` は **二層構造** で足を読む（`wbcore.data.feed.BarFeed`）:

| 区間 | 足の出どころ |
|---|---|
| 基準足が保存されている範囲 | 基準足から合成（`wbcore.data.resample`。始値＝最初・高値＝最大・安値＝最小・終値＝最後・出来高＝合計。区切りは取引所の寄付に揃える: NYSE の 1 時間足は 09:30, 10:30, …） |
| それより前 | その間隔で直接取った足（日足なら数十年） |
| 両方ある区間 | 直接取った足を優先（日足は分割調整済み、分足は無調整のため） |

分足だけを唯一の取り込み元にしないのは、遡れる期間が短いから（yfinance の 1 分足は 7 日、5〜30 分足は 60 日）。日足戦略の 30 年ぶんは分足からは作れない。

戦略は判断中に粗い足を見られる（マルチタイムフレーム）: `ctx.resample("7203", Interval.H1)` は見えている 5 分足から 1 時間足を合成し、まだ閉じていない最後の足は落とす（`completed_only=False` で形成中の足も含められる）。未来の足は構造的に混ざらない。

### 日中足の無い期間は日足から見立てる

分足が取れる前の期間や、取得元が日足しか返さない期間は、読み出すときに日足 1 本を「その日の引けに閉じる 1 本の日中足」に見立てて補う（`synthetic = True` の列が付く）。**保存はしない。** 保存されるのは取得元から来た本物の足だけで、`data status` に出るのも本物だけ。見立ては連続した系列が要る指標のウォームアップと履歴の切れ目を無くすためのもので、その日の値動きの形は持たない。本物の足だけで判断したい戦略は `synthetic` 列で除ける。

### 収集専用の設定（[`config/collect/`](config/collect/)）

将来売買する可能性のある銘柄は、戦略を持たない設定で足だけ蓄積しておく。`universe.txt` に銘柄を書き足すだけで、その日から蓄積が始まる。Webull アプリのマイウォッチリストから流し込むこともできる：

```toml
[universe]
watchlists = ["*"]      # 全リスト。名前を並べれば特定のリストだけ
```

`data sync` / `data check` のたびにウォッチリストを読み、この市場の銘柄を取り込み対象に加える（アプリでリストに足せば、次の取り込みから蓄積が始まる）。読めた銘柄は `universe.txt` にも書き足して残すので、API が落ちた日も前回のリストで蓄積が続く。売買の allowlist（`run` / `backtest` の対象）には入れない——発注対象は設定に明示的に書かれたものだけ。

```bash
uv run wbjp data watchlist                                                  # リストと中身を表示
uv run wbjp data watchlist --name 日本株 --export config/collect/universe.txt --merge   # 手で書き出す場合
```取得元は `data_provider` で切り替えられ、基準足（1 分足）に対応しない取得元でも日足は必ず揃う。

```bash
uv run wbjp data sync   --config-dir config/collect --days 7
uv run wbjp data status --config-dir config/collect --interval 1m
```

1 分足は 7 日しか遡れないので cron は毎営業日、取りこぼしに備えて 1 日 2 回。**`wbjp run` は使わない**（戦略が無くても建玉のストップ判定まで進む）。

取り込みが止まっていることに 7 日以上気づかないと、その穴は永久に残る。`data check` が「最後にいつ取れたか」と「取れているべき日に穴が無いか」を調べて、問題があれば exit 1 と通知を出す。取引日は「その銘柄の日足があった日」で決めるので、祝日の一覧は要らない。

```bash
uv run wbjp data check --config-dir config/collect            # 問題があれば exit 1
uv run wbjp data check --config-dir config/collect --notify   # WBJP_ALERT_WEBHOOK_URL に通知
```

```cron
CRON_TZ=Asia/Tokyo
# stderr も JSONL と同じ WBJP_LOG_DIR に残す（ログの置き場は 1 箇所）
0 12,16 * * 1-5 cd /opt/wbjp && .venv/bin/wbjp data sync --config-dir config/collect --days 7 >> data/logs/collect.log 2>&1
15 16 * * 1-5   cd /opt/wbjp && .venv/bin/wbjp data check --config-dir config/collect --notify >> data/logs/collect.log 2>&1
```

`data sync` 自体が失敗したとき（取得元の障害など）も同じ Webhook に通知する。Webhook は Slack / Discord の Incoming Webhook URL を `WBJP_ALERT_WEBHOOK_URL` に置く。未設定ならエラーログに残るだけ。

### 日中足で判断する

戦略とバックテストは足の間隔に依存しない。設定の `universe.interval` を変えるだけで、同じ経路が 5 分足でも回る。

```toml
[universe]
interval = "5m"

[[strategies]]
name = "intraday_sma_cross"
fast = "15m"                                # 窓は時間で書く。5分足なら 3 本、1分足なら 15 本に自動で直す
slow = "1h"
session = { start = "09:30", end = "14:30" } # 取引所の現地時刻。外では新規に建てない
flat_before = "15:00"                        # 以降は持ち越さない
```

```bash
uv run wbjp data sync --config-dir config/intraday --days 30   # universe.interval に従って 5 分足を取る
uv run wbjp backtest --config-dir config/intraday --from 2026-08-01
```

仕組み:

- 戦略は `intervals` で対応する足を宣言する。既定はすべて（指標は「本数」なので間隔に依存しない）。日付の意味に依存する戦略（`momentum_rank` の月次入れ替え、`ross_cameron` の前日比ギャップ）は日足のみで、5 分足の設定で使おうとすると起動時に弾かれる
- 窓を時間で持つ戦略は `bind(interval)` で本数に直す（`Interval.bars_in("1h")`）。`StrategyContext.at` に足の時刻（UTC）、`Market.timezone` で現地時刻
- エンジンは足を「鍵」（日足なら日付、日中足なら時刻）で並べて回す。約定は常に次の足の寄付。差金決済の当日判定・待機資金の利息・時間切れの営業日数だけを暦日の変わり目で扱う
- `--engine backtrader` は日足のみ

> **ライブ運用は日足のみ。** 日中足の設定で `wbjp run` を起動すると明示的に止まる。5 分ごとに回すには「新しい足が確定したときだけ判断する」エポック管理と実行の重なりを防ぐロックが要り、これは次の段階（[cron の節](#cron-で回す)を参照）。

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
Broker                WebullBroker / PaperBroker
```

この構造が効くところ:

- **冪等性** — 「差分だけ発注」なので、クラッシュ後に再実行しても二重発注しない
- **テスト容易性** — 戦略は純粋な関数に近く、モック不要で単体テストできる
- **同じコードがバックテストでも本番でも動く** — `Broker` を差し替えるだけ

---

## 技術スタック

| 領域 | 選定 | 理由 |
|---|---|---|
| 言語 | Python 3.14 | SDK が `<3.15` のため上限。全依存のcp314ホイールを確認済み |
| パッケージ管理 | uv | ランタイムごと管理。ロックファイルで再現性 |
| データフレーム | polars | インジケーターは polars 式で自前実装 |
| 価格データ | yfinance | 日足スイングには十分。取得済みは必ずローカルキャッシュ |
| 状態の保存 | SQLite | 注文・シグナル・実行履歴。ACID と冪等性の担保 |
| 時系列の保存 | Parquet + DuckDB | 足データの高速な集計・分析 |
| 発注 | webull-openapi-python-sdk | 公式SDK |

---

## セットアップ

```bash
uv sync
```

`uv` が Python 3.14 を自動で取得する。システムのPythonには一切触らない。

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
| 4 | 公開テスト口座 | UAT のみ。認証情報が無いときの自動フォールバック |

変数名は `WBJP_<ENV>_APP_KEY` / `_APP_SECRET` / `_ACCOUNT_ID`（例: `WBJP_PROD_APP_KEY`）。
どこから読まれているかは `wbjp credentials check --env prod` の「取得元」で確認できる。

### ローカル開発（macOS など）

```bash
uv run wbjp credentials set --env uat   # キーチェーンに保存
```

### Linux サーバー

ヘッドレスな Linux には SecretService も D-Bus も無く keyring が使えないため、
秘密は systemd 経由で環境変数として渡すのが堅い。

```bash
sudo install -d -m 750 -o root -g wbjp /etc/wbjp
sudo install -m 640 -o root -g wbjp /dev/null /etc/wbjp/wbjp.env
sudo vi /etc/wbjp/wbjp.env   # WBJP_PROD_APP_KEY=... 等
```

```ini
# /etc/systemd/system/wbjp.service
[Service]
User=wbjp
EnvironmentFile=/etc/wbjp/wbjp.env
WorkingDirectory=/opt/wbjp
ExecStart=/opt/wbjp/.venv/bin/wbjp run --live
```

リポジトリ内の `.env` は `WBJP_ENV` やログ設定など秘密でない項目に使う。
`.env` に秘密を書く場合は `chmod 600` すること（緩ければ起動時に警告が出る）。

`.env` は `dotenv_values()` で読んでおり、値は `os.environ` に**入れない**。
`load_dotenv()` と違って子プロセスや `/proc/<pid>/environ` から秘密が読めない。

---

## 使い方

```bash
# UAT（実弾なし・公開テスト口座）
WBJP_ENV=uat uv run wbjp account
WBJP_ENV=uat uv run wbjp run --live

# バックテスト
uv run wbjp backtest --from 2023-01-01 --to 2026-06-30
# 約定を Backtrader（Cerebro/Broker）に任せた第2エンジンで突き合わせる。
# 判断ロジックは同一なので、成行なら自前エンジンと約定が一致するはず。
# 差が出たらどちらかの約定モデルにバグがある（指値だけは判定基準が違うので差が出る）
uv run wbjp backtest --from 2023-01-01 --to 2026-06-30 --engine backtrader

# 本番（--live なしは常に dry-run）
WBJP_ENV=prod uv run wbjp run
WBJP_ENV=prod uv run wbjp run --live
```

---

## cron で回す

`wbjp run` は**1サイクルだけ**実行して終了する。定期実行はここに書く cron が担当する。

日足で判断するため、走らせるのは**大引け後に1日1回**。yfinance の当日足が
確定するまで少し待つので、16:00 JST 以降にしておく。

```cron
# 平日 16:30 に日次サイクルを実行
CRON_TZ=Asia/Tokyo
# stderr も JSONL と同じ WBJP_LOG_DIR に残す（ログの置き場は 1 箇所）。systemd なら journald でよい
30 16 * * 1-5 cd /opt/wbjp && WBJP_ENV=prod /opt/wbjp/.venv/bin/wbjp run --live --yes >> data/logs/wbjp-run.log 2>&1
```

cron で詰まりやすい点:

- **`--yes` が要る。** 本番の `--live` は確認プロンプトを出すが、cron には
  stdin が無い。`--yes` が無いと理由をログに残して exit 1 で止まる
  （黙って Abort はしない）
- **`cd` が要る。** `config/` `data/` `.env` はカレントディレクトリ基準。
  cron は `$HOME` で起動するので、`cd` しないと設定が見つからない。
  `cd` を使わないなら `WBJP_ENV_FILE=/etc/wbjp/wbjp.env` で `.env` を
  絶対パス指定できる
- **フルパスで呼ぶ。** cron の `PATH` は最小限。`uv run` ではなく
  `.venv/bin/wbjp` を直接叩く
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
src/wbcore/              共通基盤（wbjp / accum のどちらからも使う。逆向きの依存は無い）
├── credentials.py       認証情報の解決（キーチェーン / 環境変数 / .env。秘密はここを通す）
├── settings.py          環境変数由来の設定（WBJP_ENV など）と実発注の可否判定
├── registry.py          名前 → クラス の登録簿（両プロジェクトの戦略登録に使う）
├── logging.py           構造化ログ + 秘匿情報マスク
├── domain/
│   ├── models.py        Bar / Signal / Order / Position（すべて Decimal）、決定論的な注文ID
│   ├── market_rules.py  JP / US の取引ルールの抽象化
│   └── jp_rules.py      呼値・値幅制限・単元株・取引時間・差金決済
├── data/                MarketDataProvider → yfinance / Webull / CSV / Parquet+DuckDB / EDGAR 13F
├── indicators/ohlcv.py  polars 式で SMA/EMA/RSI/ATR/MACD/BB/ADX/Donchian
└── broker/              Broker ABC → Webull / Paper / レート制限 / 接続の組み立て（factory）

src/wbjp/                スイング売買
├── config.py            settings.toml / strategies.toml（ユニバース・リスク・出口・レジーム）
├── strategy/            Strategy ABC / 合成器4種 / サンプル戦略7本
├── portfolio/sizer.py   equal_weight / fixed_notional / atr_risk
├── risk/                リスク上限 / ストップの合成
├── engine/              リコンサイル / バックテスト / 日次ランナー
├── db/                  SQLite の判断ジャーナル
└── cli.py               `wbjp`

src/accum/               積立
├── config.py            accum.toml（戦略と銘柄の対応・予算・発注）
├── tactics.py           Tactic ABC / constant / bear_stack / stack_ladder / drawdown_ladder
├── stack.py, window.py  移動平均の配列判定 / 発注時間帯
├── plan.py              日足と戦略 → 日ごとの投下額
├── simulate.py, basket.py  検証（対照群との比較 / 複数銘柄への配分・13F）
├── execute.py           投下額 → 成行の買い注文（wbcore.broker へ渡す）
└── cli.py               `accum`
```

### 実装上の注意点（実機で確認した挙動）

| 箇所 | 内容 |
|---|---|
| `logging.py` | SDK は起動時に**自前のログハンドラとログファイル**を作り、マスクを迂回する。`harden_third_party_logging()` と `suppress_sdk_own_logging()` で塞いでいる（`ApiClient` を `TradeClient` / `DataClient` に渡す前に必ず呼ぶ） |
| `broker/webull.py` | 注文照会は**コンボ構造**（実データは入れ子の `orders` 配列）。外側だけ読むと空の注文に見える |
| `broker/webull.py` | `TradeClient()` はコンストラクタでネットワークを叩くため遅延初期化 |
| `jp_rules.py` | 呼値は「以下」区分、値幅制限は「未満」区分。引き方が違う |
| `indicators/ohlcv.py` | `diff()` の先頭 null は `pl.when` で明示的に通す。0 に化けると Wilder 平滑化の種が汚れて TA-Lib と値がずれる |
| `engine/backtest.py` | 指標は全期間で一度だけ計算する（指標が因果的なので等価）。毎日再計算すると約50倍遅い |
| `broker/paper.py` | 注文の失効は**約定処理のあと**。先に失効させると注文が一度も約定しない |
| `engine/bt_engine.py` | Backtrader の DAY 注文は日足だと翌バーの前に失効して一度も約定しないため、GTC で出して橋渡し側が翌日に取り消す。指値はバー内の高安で約定判定される（PaperBroker は寄付だけ）ので、突き合わせは成行で行う |

## 開発

```bash
uv run pytest              # ネットワークを使うものは既定でスキップ
uv run pytest -m network   # yfinance 実接続
uv run ruff check .
uv run mypy                # strict
```

## 次にやること

1. `config/settings.toml` の `universe.symbols` を実際に売買したい銘柄に変える
   （TOPIX500 構成銘柄は `topix500_symbols` にも入れる。呼値が変わる）
2. `risk.max_order_value_jpy` を自分の資金規模に合わせる
   （既定の50万円は保守的。UAT の9,800万円口座では全注文が上限に当たる）
3. UAT で `wbjp run --live` を数日回し、`wbjp explain <run_id>` で判断を目視検証
4. 本番のAPIキーを申請 → `wbjp credentials set --env prod`
5. `WBJP_ENV=prod wbjp run`（dry-run）を数日回してから `--live` に進む

