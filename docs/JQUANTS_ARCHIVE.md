# J-Quants データの蓄積（設計）

Standard プランで取れるデータを**全部**ローカルに溜め、オフラインで戦略を検討できるようにする。
これは設計の記録で、実装はまだ無い。実装したら「状態」の節を更新する。

## 何のために

- 足以外のデータ（財務・信用残・空売り・投資部門別・指数・EDINET）を使った戦略を検討したい
- API は 10 年しか遡れない。**今溜め始めないと 10 年より前は永久に取れない**（溜めれば手元の履歴は伸び続ける）
- 検討はオフラインで回したい。API の 120 回/分と契約の有無に、研究の速度を縛られない
- 過去に「その時点で何が見えていたか」を再現したい（財務は開示日で、銘柄一覧は日付で持つ）

## 前提（2026-08-31 時点の仕様）

| 項目 | 値 |
|---|---|
| API | V2、`https://api.jquants.com/v2`、`x-api-key` |
| プラン | Standard: 過去 10 年、120 回/分、当日データあり、CSV 一括ダウンロード可 |
| 応答 | `{"data": [...], "pagination_key": "..."}`。数値も文字列で来ることがある（財務） |
| 一括 | `/bulk/list`（`endpoint` か `date`）→ `/bulk/get`（`key`）→ 署名付き URL（5 分有効、使い捨て）→ `*.csv.gz`。**月次ファイル**（例 `equities_bars_daily_202501.csv.gz`） |
| 差分 cursor | `fins/summary` の `cursor` は Premium のみ。Standard は日付で取る |

### Standard で取れる端点と、取り方

「鍵」は、そのデータで 1 レコードを一意にする列。上書き（後勝ち）の単位になる。

| 端点 | 内容 | 更新（JST） | 増分の取り方 | 鍵 | 一括 |
|---|---|---|---|---|---|
| `/markets/calendar` | 取引カレンダー | 不定期（3 月末に翌年分） | `from/to` で全期間を毎回取り直す（小さい） | `Date` | 最新 1 ファイルのみ |
| `/equities/master` | 上場銘柄一覧（業種・市場区分） | 17:30 / 翌 8:00 | `date=` で**その日の一覧**。**日付ごとに残す**（上場廃止・市場変更を再現するため） | `Date, Code` | ○ |
| `/equities/bars/daily` | 株価四本値（日通し） | 16:30 | `date=` で全銘柄 1 回 | `Date, Code` | ○ |
| `/indices/bars/daily` | 指数四本値 | 16:30 | `date=` で全指数 1 回 | `Date, Code` | ○ |
| `/indices/bars/daily/topix` | TOPIX | 16:30 | `from/to` | `Date` | ○ |
| `/fins/summary` | 財務情報（決算短信サマリ） | 18:00 速報 / 24:30 確報 | `date=`（開示日）で 1 回。**確報で差し替わる**ので前日ぶんも取り直す | `DiscDate, DiscTime, Code, DiscNo`（同日複数開示あり） | ○ |
| `/fins/earnings-date` | 決算発表予定日 | 10:05 | `date=`（公表日） | `PubDate, Code` | ○ |
| `/equities/earnings-calendar` | 決算発表予定（3・9 月期） | 19:00 | パラメータ無し。取れた全件を日付で残す | `Date, Code` | — |
| `/equities/investor-types` | 投資部門別（週次） | 第 4 営業日 18:00 | `from/to`（公表日）。過誤訂正があるので 8 週ぶん重ねる | `PubDate, StDate, EnDate, Section` | ○ |
| `/markets/margin-interest` | 信用取引週末残高 | 第 2 営業日 16:30 | `date=`（金曜日付） | `Date, Code` | ○ |
| `/markets/margin-alert` | 日々公表信用残高 | 16:30 | `date=` | `PubDate, Code, AppDate` | ○ |
| `/markets/short-ratio` | 業種別空売り比率 | 16:30 | `date=` | `Date, S33` | ○ |
| `/markets/short-sale-report` | 空売り残高報告 | 17:30 | `disc_date=` | `DiscDate, CalcDate, Code, SSName, FundName` | ○ |
| `/derivatives/bars/daily/options/225` | 日経 225 オプション | 16:30 | `date=` | `Date, Code` | ○ |
| `/edinet/major-shareholders` | 大株主（EDINET） | 平日 8:00–18:00 | `date=`（提出日） | `DocId` | — |
| `/edinet/cross-shareholdings` | 政策保有株式 | 同上 | `date=` | `DocId` | — |
| `/edinet/large-volume-shareholders` | 大量保有報告 | 同上 | `date=` | `DocId` | — |

Standard で**取れない**（設計に入れない）: 前場四本値、売買内訳、財務諸表 BS/PL/CF（`/fins/details`）、配当金、先物、個別オプション、ティック（アドオン。後述のとおり取らない）、適時開示（アドオン）。分足はアドオンを契約して取る（「分足（アドオン）」の節）。

## 設計

### 方針（この 5 つで決まる）

1. **生のまま残す。** 応答の列名・値を変換せずに保存する（`AdjC` は `AdjC` のまま、文字列の数値も文字列のまま）。整形は読むときに行う。理由: 変換ロジックのバグでアーカイブが壊れると取り直せない（10 年より前は消える）。また列の意味を知らない将来の戦略が困らない。
2. **端点 × 月の Parquet。** `data/jquants/<端点>/<YYYY-MM>.parquet`。一括ダウンロードの粒度と同じなので、初回取り込みは変換だけで済む。1 ファイル 1 か月なので、日次の増分は「その月のファイルを読み、鍵で上書きして書き戻す」で足りる（月内 4,000 銘柄 × 20 日 ≈ 8 万行、一瞬）。
3. **鍵で後勝ち。** 同じ鍵のレコードは新しい取り込みが勝つ。財務の速報→確報、過誤訂正、取り直しがすべてこの一つの規則で片付く。冪等なので何度実行してもよい（cron 前提）。
4. **取り込みの記録は SQLite の台帳に。** `data/jquants/ledger.db` に「端点・対象日・取得時刻・件数・応答のハッシュ」を残す。「どこまで取れているか」「どの日が欠けているか」「訂正で何件変わったか」はここで答える。Parquet の中身を舐めて推測しない。
5. **足の読み出しは既存の抽象を通す。** `BarStore`（`data/bars/*.parquet`）は残し、`JQuantsProvider` はこのアーカイブから読む経路も持つ（後述）。戦略・エンジンから見える形は変えない。

### 置き場

```
data/jquants/
├── ledger.db                          取り込み台帳（SQLite）
├── markets_calendar/all.parquet       小さいので 1 ファイル
├── equities_master/2026-08.parquet    日付付きの銘柄一覧（月次）
├── equities_bars_daily/2016-09.parquet … 2026-08.parquet
├── indices_bars_daily/…
├── indices_bars_daily_topix/…
├── fins_summary/…                     月は DiscDate で切る
├── fins_earnings_date/…               PubDate
├── equities_earnings_calendar/…       Date
├── equities_investor_types/…          PubDate
├── markets_margin_interest/…
├── markets_margin_alert/…             PubDate
├── markets_short_ratio/…
├── markets_short_sale_report/…        DiscDate
├── derivatives_bars_daily_options_225/…
├── edinet_major_shareholders/…        SubDate
├── edinet_cross_shareholdings/…
└── edinet_large_volume_shareholders/…
```

端点名はパスの `/` を `_` にしたもの。月を切る日付列は端点ごとに決める（表の「鍵」の先頭の日付）。

型: 日付列は `Date`、**それ以外はすべて `String`**。API は数値を文字列で返すことがあり、一括 CSV は全部文字列なので、型を揃えようとすると経路で食い違う。数値が要る読み手は `typed()`（数値に解釈できる列だけ Float64 にする）を通す。null は null のまま。
列が増えた（API の仕様変更）ときは足すだけ、減ったときは null で埋める（`diagonal` 結合）。**列は落とさない**。

### 台帳（`ledger.db`）

```sql
CREATE TABLE ingest (
  endpoint    TEXT NOT NULL,   -- "/equities/bars/daily"
  target      TEXT NOT NULL,   -- 対象（"2026-08-28" / "2026-08" / "all"）
  source      TEXT NOT NULL,   -- "api" | "bulk"
  fetched_utc TEXT NOT NULL,   -- ISO 8601
  rows        INTEGER NOT NULL,
  changed     INTEGER NOT NULL,-- 鍵で上書きして実際に増減・変化した行数
  digest      TEXT NOT NULL,   -- 応答の sha256（同じ内容の取り直しを検出）
  run_id      TEXT NOT NULL,   -- ログの run_id と突き合わせる
  PRIMARY KEY (endpoint, target, fetched_utc)
);
```

「対象日 D の bars を最後に取ったのはいつか」「D はまだ一度も取っていないか」「確報で何行変わったか」がこの表で分かる。

### 取り込みの流れ

**初回（バックフィル）— 一括ダウンロードを使う。**
`/bulk/list?endpoint=…` で月次ファイルを列挙 → `/bulk/get?key=…` → URL（5 分）→ `csv.gz` を落として Parquet に変換 → 台帳に `source=bulk`。
10 年 × 12 か月 × 約 12 端点 ≈ 1,500 ファイル。レート制限 120 回/分なら list+get で 30 分程度。**API を日付で 2,500 日ぶん叩く必要は無い**。
一括に無い端点（`earnings-calendar`、EDINET 3 種）は API を日付で回す（EDINET は提出日ごと。10 年ぶん ≈ 2,500 日 × 3 端点 → 1 時間強。夜に 1 回）。

**日次（増分）— API を日付で叩く。** 1 端点 1 日 1 リクエスト、全部で 15 回程度。

```
毎営業日 19:00 JST  bars / indices / topix / master / short-ratio / margin-alert / opt225 / short-sale-report / edinet 3 種 / earnings-date / earnings-calendar
毎営業日 09:00 JST  fins/summary を「前日と前々日」の開示日で取り直す（24:30 の確報を拾う）
毎週 木曜 19:00     investor-types（直近 8 週）/ margin-interest（直近 2 週）
毎月 1 日           calendar 全期間 / 前月ぶんを全端点で取り直し（訂正の取りこぼし保険。changed が 0 なら何もしない）
```

cron は今と同じ「固定間隔で叩き、必要かどうかは中で判断する」。判断材料は台帳（その日の取り込みが無い、または前回から N 時間過ぎている）と取引カレンダー（休場日は bars を取りに行かない）。

**取り直しの重ね幅**（訂正に備える）: bars・indices は 5 営業日、fins は 2 日、investor-types は 8 週。`BarStore.OVERLAP_DAYS` と同じ考え方。

### 既存の足データとの関係

`JQuantsProvider.fetch_bars` は今 API を直接叩く。アーカイブができたら次の順で読む:

1. `data/jquants/equities_bars_daily/` に要求範囲がすべて有れば、そこから（API を叩かない。オフラインで動く）
2. 無い部分だけ API から取り、**アーカイブにも書く**（`accum sync` が副産物として蓄積に貢献する）

`BarStore`（`data/bars/*.parquet`、正規スキーマ）は戦略の入口として残す。二重に持つが、正規スキーマの側は「戦略が読む形」、アーカイブは「取得元の形」で役割が違う。容量は問題にならない（bars 10 年で数百 MB）。

### 読み出し（オフラインでの検討）

- DuckDB: `jquants query "SELECT … FROM read_parquet('data/jquants/equities_bars_daily/*.parquet')"`。月ファイルなので期間で絞れば必要な分しか読まない
- DuckDB: `BarStore.query` と同じく、端点ごとにビューを張る補助を用意する（`jquants.bars`、`jquants.fins` …）。研究ノートから SQL で横断できる
- 「その時点で見えていた財務」は `fins_summary` を `DiscDate <= 判定日` で絞って `Code` ごとに最新 1 件を取る。ルックアヘッドを避ける定型なので関数にする（`as_of(frame, date)`）

### CLI（案）

`wbjp` / `accum` と同じ構成で、蓄積専用の入口を切る（保管庫は `pkg/jquants/archive`、CLI は `cmd/jquants/`、コマンド名は `jquants`。`jq` は JSON ツールと衝突するので使わない）。

```
jquants sync [--days N] [--only 端点]   台帳を見て必要な端点・日付だけ取る（cron 用。冪等）。--days で未取得日を遡って埋める
jquants backfill [--since 2016-09]     一括ダウンロードで初回取り込み。再実行は更新ファイルだけ
jquants status                         端点ごとの月数・最古・最新・最終取得
jquants check [--date D] [--days 30]   営業日の欠けを探す（あれば非 0 で終了。監視用）
jquants query "SELECT …"               DuckDB で端点名のビューを張って SQL（研究用）
```

ログは `docs/LOGGING.md` の規約どおり `jquants-<env>.jsonl` に。`code` は `jquants.ingest`（端点・対象・rows・changed・source）と `jquants.gap`（欠け検出）。

### レート制限（120 回/分）

- **送る前に間隔を空ける**（`JQuantsClient` の `Throttle`、既定 100 回/分＝0.6 秒間隔）。バックフィルや EDINET の遡り（数千リクエスト）はこれで上限内に収まり、429 を「起こさない」のが基本
- **それでも 429 が返ったら** `Retry-After`（無ければ 60 秒）待って再試行（最大 8 回）。窓が 1 分なので数秒の指数バックオフでは足りない
- 署名付き URL からの `csv.gz` ダウンロードは API の回数に数えない（別ホスト）
- 上限はプロセス内で守る。`accum sync` と `jquants sync` が同時に走ると合計で超えうるため、既定を 120 ではなく 100 にして余白を残す。cron で同時刻に並べない

### 冪等性・安全

- 同じ日を何度取っても結果は同じ（鍵で後勝ち）。cron の重複起動は `flock` で防ぐ（今と同じ）
- Parquet の書き戻しは一時ファイルに書いてから `rename`。途中で落ちても壊れたファイルは残らない
- 一括ダウンロードの CSV は変換後も `data/jquants/_raw/<端点>/<file>.csv.gz` に**残す**（変換にバグがあっても API を叩き直さずに済む。10 年より前が消える問題の最後の保険）
- API キーは既存の `WBJP_JQUANTS_API_KEY`。ログには出ない（`register_secret` 済み）
- バックアップ: `data/jquants/` は `accum backup` と同じ仕組みで別の場所に複製する（台帳は小さいので毎日、Parquet は週次）

### 容量とコストの見積もり

| データ | 行数（10 年） | Parquet |
|---|---|---|
| bars daily | 4,000 銘柄 × 2,450 日 ≈ 1,000 万 | 300–500 MB |
| master（日次で保存） | 4,000 × 2,450 ≈ 1,000 万 | 100 MB（文字列が多いが辞書圧縮が効く） |
| fins summary | 4,000 × 4 期 × 10 年 ≈ 16 万 | 30 MB |
| その他全部 | | 100 MB 程度 |
| 生 CSV（保険） | | 1–2 GB |

日次の通信は 15 リクエスト程度（レート制限の 1 分ぶんにも満たない）。

## メモリ

**保管庫は 1 端点 10 年で 1,000 万行になるので、「全期間を読む」経路を作ってはいけない。**
`Frame` の 1 行は列に揃えた `[]*string`（`Row`）で、実測 **1 セルおよそ 44 バイト**（15 列で 660 B/行）。
Parquet 上では数十バイトの行が、メモリでは 10〜40 倍に膨らむ。bars の 10 年ぶんを丸ごと `Frame` に載せると 7GB を超える。
2026-09-04 までは行が `map[string]*string` で 1 セル 96 バイトだった（下の「状態」）。

### 読み方の決まり

| 欲しいもの | 使うもの | 5 年（480 万行）での実測 |
|---|---|---|
| 保存されている日付だけ | `Dates()` | 0.08 秒・常駐 5.5MB |
| 1 銘柄・数列ぶん | `ReadWhere` に `Columns` と `Keep` | 0.27 秒・常駐 33MB |
| ある月・ある日ぶん | `Read(ep, start, end)` | 0.08 秒・常駐 114MB（1 か月＝8 万行） |
| 全期間・全列 | **無い**（`Scan` は上限に当たって止まる） | — |

- `Dates()` は日付列だけを Parquet の列単位で読む。行を組み立てないので、10 年でも常駐は日付の種類ぶん（2,500 個）。
  ページの統計で `min == max` のページは値を読まずに済ませる（日付順に書いているのでほぼ毎回効く）
- `ReadWhere` の `Keep` は **`Frame` に載る前**の行（`RowView`）に対して呼ばれる。落とした行には文字列も map も作らない。
  `RowView.Equal` / `HasPrefix` は値を複製せずに比べる。判定用の列は `Columns` に入れなくてもよい
- `Columns` で指定しなかった列は文字列に起こさない（ページのバッファも引きずらない）。日付列は黙って足される
- 月ファイルは最大 4 本並行で読む。絞り込みを押し下げてあるので、並行にしても常駐は増えない
- `Scan()`（全期間・全列）は取引カレンダーのような小さい端点だけ。行数の多い端点に使うと下の上限に当たる

### 上限

1 回の読み出しが `Frame` に載せていい量に上限がある（既定 2GiB、`JQUANTS_READ_BUDGET_MB` で変更）。
超えると **OOM で殺される代わりに**、どの端点で何 MB になったか・何を絞れば通るかを書いたエラーで止まる。
cron では `GOMEMLIMIT`（Go のヒープの soft 上限）と `GOGC` も併せて渡す（`deploy/crontab.txt`）——
上限に余裕があるうちは GC を控えて速く動き、近づいたら GC が先に頑張る。

### 書き込み

`Upsert` は月ファイル単位（8 万行）で読み直して書き戻すので、1 か月ぶん（bars で約 300MB）が上限。
`writeParquet` は 8,192 行ずつ書く（全行ぶんの `[]parquet.Row` を作らない）。
`sortByKey` は鍵を行ごとに 1 回だけ組む（比較関数の中で組むと 8 万行で 270 万回のアロケートになる）。
`DigestOf` / `countChanged` は行ごとの正規化文字列を並べずに 32 バイトのハッシュだけを持つ
（正規化文字列を全行ぶん並べると表と同じ量のメモリをもう一度使う）。`CSVToFrame` は gzip を流しながら読み、
1 行の各フィールドは行ごとの 1 本の文字列の部分文字列にする（フィールドごとに複製しない）。

### ベンチマーク（`go test ./pkg/jquants/archive/ -bench . -benchmem`）

1 か月ぶんの日足を模した 5.2 万行 × 15 列。行を map からスライスに変え、セルごとのアロケートをやめた前後:

| 経路 | 変更前 | 変更後 |
|---|---|---|
| 全列の読み出し | 51 ms / 78 MB / 177 万 alloc | 32 ms / 35 MB / 89 万 alloc |
| 3 列に射影した読み出し | 32 ms / 25 MB | 23 ms / 14 MB |
| `DigestOf` | 24 ms / 36 MB | 24 ms / 1.7 MB |
| 月の書き戻し（`Upsert` 2 回目） | 190 ms / 191 MB | 149 ms / 112 MB |
| `CSVToFrame`（一括 CSV） | 48 ms / 85 MB | 19 ms / 32 MB |

## 分足（アドオン）

2026-01 に追加された有料アドオン（Light 以上、月額）で株価の分足とティックが取れる。**分足だけ取る。ティックは取らない**（下の理由）。これは設計の記録で、実装はまだ無い。

### 仕様（2026-09-03 時点、公式リファレンスより）

| | 株価分足 `/equities/bars/minute` | 株価ティック `/equities/trades` |
|---|---|---|
| 取り方 | API（`code=` か `date=` が必須。`from/to`、`pagination_key`）＋ 一括 CSV | 一括 CSV のみ（API なし） |
| 列 | `Date, Time(HH:mm), Code, O, H, L, C, Vo, Va`。数値は JSON の number | `Date, Code, Time(HH:MM:SS.ffffff), SessionDistinction, Price, TradingVolume, TransactionId` |
| 履歴 | **2 年** | **2 年** |
| 更新 | 日次（リアルタイムではない） | 日次 |
| 備考 | 約定の無い分は行が無い（疎）。東証上場の現物のみ | 東証上場のみ |

一括ファイルが月次か日次かはリファレンスに書かれていない。初回の `bulk_list` で確かめる（`_month_in` は `YYYYMMDD` でも先頭 6 桁を拾うのでどちらでも動く）。

**履歴が 2 年しか無い**ので、日足以上に「溜め始めないと消える」。台帳・一括バックフィル・`_raw` の保険はそのまま使う。

### 規模（合成データでの実測）

4,000 銘柄 × 約 300 分 ＝ 上限 120 万行/日。疎なので実際は 100 万行弱。数値型で zstd に落として **約 9MB/日、年 2GB 前後**。日足 10 年ぶん（1,000 万行・298MB）を 1〜2 週間で超える。

### 日足の `Archive` の流儀をそのまま使わない理由

| 日足の設計 | 日足では | 分足では |
|---|---|---|
| 全列 `String`、読むとき `typed()` | 容量ペナルティなし（辞書圧縮）。cast は 1,800 万行で 0.15 秒 | cast のコストとメモリが行数に比例し、読むたびに億単位の文字列→数値変換になる。API は number で返し CSV も同じ形に `cast` できるので、文字列で持つ理由（経路の食い違い）が無い |
| 端点 × 月の Parquet を読み直して書き戻す（`_upsert_month`） | 月 9 万行なので一瞬 | 月 2,400 万行・170MB を毎日読んで `unique`・`sort`・再書き込み。7 秒/日・メモリ数 GB、月末ほど重い |
| `BarStore`（1 銘柄 1 ファイル、丸ごと書き直し） | 日足専用のスキーマ（`date` 鍵） | 使わない。日足のまま残す |

### 設計

1. **置き場は 1 営業日 1 ファイル。** `data/jquants/equities_bars_minute/<YYYY-MM-DD>.parquet`。取得の単位（`date=` か一括の 1 日ぶん）とファイルの単位が一致するので **append-only**——その日が揃ったら不変、訂正はその日のファイルを丸ごと差し替える。既存ファイルは読み直さない。
2. **型を固定して書く。** `Date: Date`, `Time: String`（生のまま）, `Code: String`, `Datetime: Datetime("ms", "Asia/Tokyo")`（`Date`+`Time` から作る派生列。読み手はこれを使う）, `O/H/L/C: Float64`, `Vo/Va: Int64`。API（number）と CSV（文字列）はどちらもこのスキーマに `cast` する。列が増えたら足す・減ったら null（`diagonal`）は日足と同じ。
3. **ファイル内は `(Code, Datetime)` でソート、行グループ 5〜10 万行。** 1 銘柄の抜き出しは行グループの統計で刈れる（1 日ぶんで数ミリ秒）、ある時刻の全銘柄横断は 1 ファイル読むだけ（0.02 秒）。デイトレの選定のような「日付で横断」がこのプロジェクトの主な読み方なので、銘柄分割ではなく日付分割にする。1 銘柄 1 年ぶんは 245 ファイルを開くが、各ファイルからは数 KB しか読まない。
4. **`Archive` を拡張して載せる。** `Endpoint` に「分割の単位（月／日）」と「固定スキーマ（あれば `String` にせず `cast`）」を足す。`plan()` / `gaps()` / `sync` / `backfill` / 台帳（`endpoint="/equities/bars/minute"`, `target=日付`）はそのまま再利用する。日足の端点の挙動は変えない。
5. **日次更新は一括 CSV を優先する。** API の `date=` は全銘柄 100 万行のページングで数百リクエストになりうる（120 回/分）。一括が日次ファイルならそれを取り、月次ファイルしか無ければ当月ぶんを API の `date=` で取る。
6. `available_at` は日足と同じ 16:30 を仮置きし、実機で確かめる。

### ティックを取らない理由

東証全体で 1 日数千万約定。Parquet にしても **年 50〜100GB** で、サーバーの空き（80GB）に 2 年ぶんは収まらない。ティックを使う戦略の目処も無い。必要になったら「`_raw` の csv.gz だけ残して変換しない」か「ユニバースの銘柄だけ Parquet 化する」から始める。分足で足りるかを先に確かめる。

## 決めていないこと（実装前に決める）

1. **master を毎日残すか、変化した日だけにするか。** 毎日は単純だが 1,000 万行。「変化した日だけ」は行が 1/100 になるが「その日の一覧」を復元する処理が要る。→ まず毎日残す（単純さ優先。容量は問題にならない）。
2. **EDINET 3 種を最初から入れるか。** 一括に無く API を日付で 7,500 回叩く。戦略で使う目処が無ければ後回しでもよい。→ 日次の増分には最初から入れ、バックフィルは後回し。
3. **`BarStore` を廃止してアーカイブ直読みにするか。** 米国株（yfinance）が残る限り `BarStore` は要る。→ 残す。
4. `fins/summary` の確報反映（24:30）が実際にいつ API に乗るかは実機で確かめる。朝 9 時の取り直しで足りなければ昼にもう 1 回。

## 状態

- 2026-09-04: `Frame` の行を `map[string]*string` から列に揃えた `[]*string` に変えた（1 セル 96 → 44 バイト）。
  列名で引くときは `Get` / `AppendRow`。読み出し・CSV・ダイジェスト・書き戻しの経路でセルごとのアロケートをやめた。
  数字は「ベンチマーク」の節。
- 2026-09-03: メモリの節約。`Dates()` が全期間を `Frame` に載せていたのをやめ（bars 10 年で 15GB 超 → 数 MB）、
  `ReadWhere`（列の射影・`RowView` での行の絞り込み）を足した。読み出しに上限を入れて、
  超えたら OOM ではなくエラーで止まるようにした。「メモリ」の節を参照。
- 2026-09-03: 分足（アドオン）の設計を追記。実装は未着手。ティックは取らない。
- 2026-08-31: 実装済み（`wbcore.data.jquants_client` / `wbcore.data.jquants_archive` / `jquants` CLI）。`JQuantsProvider` はアーカイブに揃っていればそこから読み、API から取ったぶんはアーカイブに書く。**実機（Standard）での疎通は未確認**——一括 CSV の列名が API と同じ前提、`HolDiv` の値、確報の反映時刻は初回実行で確かめる。
