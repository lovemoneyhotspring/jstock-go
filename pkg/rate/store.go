package rate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// Store はレーティングの記録簿（SQLite）。
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS ratings (
    pub_date      TEXT NOT NULL,   -- 一覧に載っている日付（YYYY-MM-DD）
    code          TEXT NOT NULL,
    name          TEXT NOT NULL,
    firm          TEXT NOT NULL,
    rating        TEXT NOT NULL,   -- 原文（「買い継続」「2→3格下げ」）
    target        TEXT NOT NULL,   -- 原文（「9300円→9500円」）
    action        TEXT NOT NULL,   -- new / up / down / keep
    rating_from   TEXT NOT NULL,
    rating_to     TEXT NOT NULL,
    target_from   REAL NOT NULL,
    target_to     REAL NOT NULL,
    first_seen_at TEXT NOT NULL,   -- この行を初めて見た時刻（JST, RFC3339）
    last_seen_at  TEXT NOT NULL,
    PRIMARY KEY (pub_date, code, firm, rating, target)
);
CREATE INDEX IF NOT EXISTS ratings_first_seen ON ratings(first_seen_at);
CREATE INDEX IF NOT EXISTS ratings_firm ON ratings(firm, pub_date);

CREATE TABLE IF NOT EXISTS fetches (
    fetched_at TEXT PRIMARY KEY,   -- 取りに行った時刻（JST, RFC3339）
    rows       INTEGER NOT NULL,   -- ページに載っていた行数
    new_rows   INTEGER NOT NULL,   -- そのうち初めて見た行数
    status     TEXT NOT NULL,      -- ok / エラーの内容
    elapsed_ms INTEGER NOT NULL
);
`

// OpenStore は記録簿を開き、無ければ作る。
func OpenStore(path string) (*Store, error) {
	db, err := storage.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema + eventSchema + tradersSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("レーティング DB の作成に失敗: %w", err)
	}
	// 先に作った DB には direction が無い。既にあるときのエラーは読み飛ばす
	if _, err := db.Exec(`ALTER TABLE traders_ratings ADD COLUMN direction TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		_ = db.Close()
		return nil, fmt.Errorf("direction 列の追加に失敗: %w", err)
	}
	// direction の索引は列が揃ってから。スキーマに混ぜると、先に作った DB で
	// 「列が無い」と言われて開けなくなる
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS traders_ratings_dir ON traders_ratings(direction, pub_date)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("direction の索引作成に失敗: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// Save は取れた行を書き込み、初めて見た行数を返す。
// 既にある行は first_seen_at を変えず last_seen_at だけ進める。
// 「いつ出たか」は初出時刻でしか測れないので、ここを塗り替えてはいけない。
func (s *Store) Save(ctx context.Context, entries []Entry, seenAt time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO ratings (pub_date, code, name, firm, rating, target, action,
                     rating_from, rating_to, target_from, target_to,
                     first_seen_at, last_seen_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (pub_date, code, firm, rating, target)
DO UPDATE SET last_seen_at = excluded.last_seen_at`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	ts := seenAt.Format(time.RFC3339)
	added := 0
	for _, e := range entries {
		res, err := stmt.ExecContext(ctx, e.PubDate, e.Code, e.Name, e.Firm, e.Rating, e.Target,
			e.Action, e.RatingFrom, e.RatingTo, e.TargetFrom, e.TargetTo, ts, ts)
		if err != nil {
			return 0, fmt.Errorf("%s %s %s の書き込みに失敗: %w", e.PubDate, e.Code, e.Firm, err)
		}
		// 更新のときも RowsAffected は 1 になるので、挿入かどうかは
		// first_seen_at が今回の時刻かで見る。
		if n, _ := res.RowsAffected(); n > 0 {
			var first string
			if err := tx.QueryRowContext(ctx,
				`SELECT first_seen_at FROM ratings WHERE pub_date=? AND code=? AND firm=? AND rating=? AND target=?`,
				e.PubDate, e.Code, e.Firm, e.Rating, e.Target).Scan(&first); err == nil && first == ts {
				added++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// RecordFetch は取りに行った記録を残す。失敗した回も残す
// （取れなかった時間帯を、載っていなかった時間帯と取り違えないため）。
func (s *Store) RecordFetch(ctx context.Context, at time.Time, rows, newRows int, status string, elapsed time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO fetches (fetched_at, rows, new_rows, status, elapsed_ms) VALUES (?,?,?,?,?)`,
		at.Format(time.RFC3339), rows, newRows, status, elapsed.Milliseconds())
	return err
}

// eventSchema はレーティングの「動き」を溜める表。
//
// grail の一覧（ratings）と分けている。あちらは据え置きも載るが時刻を持たず、
// こちらは動きだけだが「いつ知れたか」がはっきりしている。
// 検証で使えるのは後者——知れた日より前には使えない。
const eventSchema = `
CREATE TABLE IF NOT EXISTS rating_events (
    feed_date  TEXT NOT NULL,   -- この情報を知れた日（配信日 YYYY-MM-DD）。07:10 配信なので当日の寄り前に使える
    pub_label  TEXT NOT NULL,   -- 見出しの「（9/9）」＝発表日の月日。前営業日ぶんなので feed_date とずれる
    code       TEXT NOT NULL,
    kind       TEXT NOT NULL,   -- rating（投資判断） / target（目標株価）
    direction  TEXT NOT NULL,   -- up / down / new
    news_id    TEXT NOT NULL,
    news_time  TEXT NOT NULL,   -- 配信時刻 HHMM
    PRIMARY KEY (feed_date, code, kind, direction)
);
CREATE INDEX IF NOT EXISTS rating_events_code ON rating_events(code, feed_date);
CREATE INDEX IF NOT EXISTS rating_events_feed ON rating_events(feed_date, direction);

CREATE TABLE IF NOT EXISTS news_days (
    feed_date   TEXT PRIMARY KEY,  -- 取り込んだ配信日
    news_count  INTEGER NOT NULL,  -- その日のニュース総数
    event_count INTEGER NOT NULL,  -- うちレーティングの動き（銘柄×種別）
    imported_at TEXT NOT NULL
);
`

// Event はレーティングの動き 1 件（銘柄 × 種別 × 方向）。
type Event struct {
	FeedDate  string
	PubLabel  string
	Code      string
	Kind      string
	Direction string
	NewsID    string
	NewsTime  string
}

// SaveEvents はレーティングの動きを書き込む。
func (s *Store) SaveEvents(ctx context.Context, day string, events []Event, newsCount int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO rating_events (feed_date, pub_label, code, kind, direction, news_id, news_time)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT (feed_date, code, kind, direction) DO NOTHING`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range events {
		if _, err := stmt.ExecContext(ctx, e.FeedDate, e.PubLabel, e.Code, e.Kind, e.Direction, e.NewsID, e.NewsTime); err != nil {
			return fmt.Errorf("%s %s %s の書き込みに失敗: %w", e.FeedDate, e.Code, e.Direction, err)
		}
	}
	// 取り込んだ日は必ず記録する。件数 0 の日（休日・配信なし）と
	// 「まだ取り込んでいない日」を区別するため
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO news_days (feed_date, news_count, event_count, imported_at) VALUES (?,?,?,?)`,
		day, newsCount, len(events), time.Now().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// ImportedDays は既に取り込んだ配信日の集合。取り込みのやり直しを避けるのに使う。
func (s *Store) ImportedDays(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT feed_date FROM news_days`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	done := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		done[d] = true
	}
	return done, rows.Err()
}

// tradersSchema は月別一覧（2005 年〜）を溜める表。
//
// 出所ごとに表を分けている。混ぜると「どこまで信じてよいか」が行ごとに変わってしまう。
const tradersSchema = `
CREATE TABLE IF NOT EXISTS traders_ratings (
    pub_date    TEXT NOT NULL,   -- 掲載日（YYYY-MM-DD）。ページが年月を持つので年の推定は要らない
    code        TEXT NOT NULL,
    name        TEXT NOT NULL,
    market      TEXT NOT NULL,   -- 東P / 東1 など。時点の市場区分
    firm        TEXT NOT NULL,
    rating      TEXT NOT NULL,   -- 原文（"Buy→Hold" "新規Buy2" "1継続"）
    target      TEXT NOT NULL,   -- 原文（"3600→2600円"）
    rating_from TEXT NOT NULL,
    rating_to   TEXT NOT NULL,
    target_from REAL NOT NULL,
    target_to   REAL NOT NULL,
    direction   TEXT NOT NULL DEFAULT '',  -- up / down / keep / new / unknown（序列に当てて出す）
    PRIMARY KEY (pub_date, code, firm, rating, target)
);
CREATE INDEX IF NOT EXISTS traders_ratings_date ON traders_ratings(pub_date);
CREATE INDEX IF NOT EXISTS traders_ratings_code ON traders_ratings(code, pub_date);

CREATE TABLE IF NOT EXISTS traders_months (
    year_month  TEXT PRIMARY KEY,
    row_count   INTEGER NOT NULL,
    imported_at TEXT NOT NULL
);
`
