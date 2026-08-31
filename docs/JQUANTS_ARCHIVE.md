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

Standard で**取れない**（設計に入れない）: 前場四本値、売買内訳、財務諸表 BS/PL/CF（`/fins/details`）、配当金、先物、個別オプション、分足・ティック（アドオン）、適時開示（アドオン）。

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

- polars: `pl.scan_parquet("data/jquants/equities_bars_daily/*.parquet")`。月ファイルなので期間で絞れば必要な分しか読まない
- DuckDB: `BarStore.query` と同じく、端点ごとにビューを張る補助を用意する（`jquants.bars`、`jquants.fins` …）。研究ノートから SQL で横断できる
- 「その時点で見えていた財務」は `fins_summary` を `DiscDate <= 判定日` で絞って `Code` ごとに最新 1 件を取る。ルックアヘッドを避ける定型なので関数にする（`as_of(frame, date)`）

### CLI（案）

`wbjp` / `accum` と同じ構成で、蓄積専用の入口を切る（保管庫は `wbcore.data.jquants_archive`、CLI は `src/jquants/`、コマンド名は `jquants`。`jq` は JSON ツールと衝突するので使わない）。

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

## 決めていないこと（実装前に決める）

1. **master を毎日残すか、変化した日だけにするか。** 毎日は単純だが 1,000 万行。「変化した日だけ」は行が 1/100 になるが「その日の一覧」を復元する処理が要る。→ まず毎日残す（単純さ優先。容量は問題にならない）。
2. **EDINET 3 種を最初から入れるか。** 一括に無く API を日付で 7,500 回叩く。戦略で使う目処が無ければ後回しでもよい。→ 日次の増分には最初から入れ、バックフィルは後回し。
3. **`BarStore` を廃止してアーカイブ直読みにするか。** 米国株（yfinance）が残る限り `BarStore` は要る。→ 残す。
4. `fins/summary` の確報反映（24:30）が実際にいつ API に乗るかは実機で確かめる。朝 9 時の取り直しで足りなければ昼にもう 1 回。

## 状態

- 2026-08-31: 実装済み（`wbcore.data.jquants_client` / `wbcore.data.jquants_archive` / `jquants` CLI）。`JQuantsProvider` はアーカイブに揃っていればそこから読み、API から取ったぶんはアーカイブに書く。**実機（Standard）での疎通は未確認**——一括 CSV の列名が API と同じ前提、`HolDiv` の値、確報の反映時刻は初回実行で確かめる。
