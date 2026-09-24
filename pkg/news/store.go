// Package news は立花証券のニュース電文を丸ごと溜める記録簿。
//
// 電文が遡れるのは 90 日だけで、止めた期間は取り返せない
// （`~/obsidian-vault/20-research/2026-09-tachibana-news-inventory.md`）。
// だから選り分けずに全ジャンルの見出しと本文を残す。何に効くかは後から決める。
package news

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// Store はニュースの記録簿（SQLite）。
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS news (
    feed_date  TEXT NOT NULL,   -- 配信日（YYYY-MM-DD）。電文に渡した日付
    news_id    TEXT NOT NULL,   -- 発信元の記事 ID（p_ID）
    time       TEXT NOT NULL,   -- 配信時刻 HHMM（発信元の値）
    genres     TEXT NOT NULL,   -- 「|」区切り（p_GNL）
    categories TEXT NOT NULL,   -- 「|」区切り（p_CGL）
    codes      TEXT NOT NULL,   -- 「|」区切りの銘柄コード（p_ISL）
    headline   TEXT NOT NULL,
    body       TEXT NOT NULL,
    saved_at   TEXT NOT NULL,   -- 初めて保存した時刻（JST, RFC3339）
    PRIMARY KEY (feed_date, news_id)
);
CREATE INDEX IF NOT EXISTS news_time ON news(feed_date, time);

-- 銘柄で引くための転置。1 記事に複数銘柄が入るので分けて持つ
CREATE TABLE IF NOT EXISTS news_codes (
    feed_date TEXT NOT NULL,
    news_id   TEXT NOT NULL,
    code      TEXT NOT NULL,
    PRIMARY KEY (feed_date, news_id, code)
);
CREATE INDEX IF NOT EXISTS news_codes_code ON news_codes(code, feed_date);

-- ジャンルの転置。1 記事に複数ジャンルが付く
CREATE TABLE IF NOT EXISTS news_genres (
    feed_date TEXT NOT NULL,
    news_id   TEXT NOT NULL,
    genre     TEXT NOT NULL,
    PRIMARY KEY (feed_date, news_id, genre)
);
CREATE INDEX IF NOT EXISTS news_genres_genre ON news_genres(genre, feed_date);

-- 取り込んだ日の台帳。取れなかった日を「無かった日」と混ぜないために、
-- 失敗も status に残す
CREATE TABLE IF NOT EXISTS news_days (
    feed_date  TEXT PRIMARY KEY,
    items      INTEGER NOT NULL,   -- 電文が返した件数
    new_items  INTEGER NOT NULL,   -- そのうち初めて見た件数
    status     TEXT NOT NULL,      -- ok / エラーの内容
    fetched_at TEXT NOT NULL
);
`

// OpenStore は記録簿を開き、無ければ作る。
func OpenStore(path string) (*Store, error) {
	db, err := storage.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ニュース DB の作成に失敗: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// Save はその日のニュースを書き込み、初めて見た件数を返す。
//
// 同じ記事が再配信されても本文が差し替わることがある（訂正）ので、
// 本文は後勝ちで上書きし、saved_at（初めて見た時刻）は変えない。
func (s *Store) Save(ctx context.Context, day string, items []broker.NewsItem, at time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	insNews, err := tx.PrepareContext(ctx, `
        INSERT INTO news (feed_date, news_id, time, genres, categories, codes, headline, body, saved_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(feed_date, news_id) DO UPDATE SET
            time = excluded.time, genres = excluded.genres, categories = excluded.categories,
            codes = excluded.codes, headline = excluded.headline, body = excluded.body`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insNews.Close() }()

	// 転置は記事ごとに消してから入れ直す。訂正で銘柄やジャンルが外れた記事に、
	// 古い行が残って引っかかり続けないように
	delCodes, err := tx.PrepareContext(ctx, `DELETE FROM news_codes WHERE feed_date = ? AND news_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = delCodes.Close() }()
	delGenres, err := tx.PrepareContext(ctx, `DELETE FROM news_genres WHERE feed_date = ? AND news_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = delGenres.Close() }()
	exists, err := tx.PrepareContext(ctx, `SELECT COUNT(*) FROM news WHERE feed_date = ? AND news_id = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = exists.Close() }()

	insCode, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO news_codes (feed_date, news_id, code) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insCode.Close() }()

	insGenre, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO news_genres (feed_date, news_id, genre) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = insGenre.Close() }()

	stamp := at.Format(time.RFC3339)
	newRows := 0
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		// 初出かどうかは書く前に同じトランザクションの中で見る。saved_at が今回の時刻かで
		// 見ると、同じ秒に 2 回保存したとき 2 回とも初出に数えてしまう。
		// 同じ応答に同じ記事が 2 度あっても、2 度目は既にあるので数えない
		var before int
		if err := exists.QueryRowContext(ctx, day, item.ID).Scan(&before); err != nil {
			return 0, err
		}
		if before == 0 {
			newRows++
		}
		if _, err := insNews.ExecContext(ctx, day, item.ID, item.Time,
			strings.Join(item.Genres, "|"), strings.Join(item.Categories, "|"),
			strings.Join(item.Codes, "|"), item.Headline, item.Body, stamp); err != nil {
			return 0, fmt.Errorf("%s の記事 %s の保存に失敗: %w", day, item.ID, err)
		}
		if _, err := delCodes.ExecContext(ctx, day, item.ID); err != nil {
			return 0, err
		}
		if _, err := delGenres.ExecContext(ctx, day, item.ID); err != nil {
			return 0, err
		}
		for _, code := range item.Codes {
			if _, err := insCode.ExecContext(ctx, day, item.ID, code); err != nil {
				return 0, err
			}
		}
		for _, genre := range item.Genres {
			if _, err := insGenre.ExecContext(ctx, day, item.ID, genre); err != nil {
				return 0, err
			}
		}
	}

	// items は取り直しで減らさない（一時的に短い応答が返っても、溜まった記事は消えない）。
	// new_items は「この回に初めて見た件数」。足し込むと取り直すたびに膨らむ
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO news_days (feed_date, items, new_items, status, fetched_at)
        VALUES (?, ?, ?, 'ok', ?)
        ON CONFLICT(feed_date) DO UPDATE SET
            items = MAX(news_days.items, excluded.items), new_items = excluded.new_items,
            status = 'ok', fetched_at = excluded.fetched_at`,
		day, len(items), newRows, stamp); err != nil {
		return 0, err
	}
	return newRows, tx.Commit()
}

// RecordFailure は取れなかった日を残す。
// 「取れなかった日」と「1 件も無かった日」を混ぜると、後から穴を埋められない。
func (s *Store) RecordFailure(ctx context.Context, day string, at time.Time, cause string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO news_days (feed_date, items, new_items, status, fetched_at)
        VALUES (?, 0, 0, ?, ?)
        ON CONFLICT(feed_date) DO UPDATE SET status = excluded.status, fetched_at = excluded.fetched_at`,
		day, cause, at.Format(time.RFC3339))
	return err
}

// DoneDays は「ok で取り込み済み」の日を返す。0 件の日（休日）も含む。
//
// 以前は items > 0 に絞っていたので、休日が 90 日ぶん毎回取り直されていた。
// 配信の前に回して 0 件を掴んだ日の取り直しは、Sync が直近 recent 日を
// 済みでも取り直すことで拾う（それより古い 0 件の日は本当に 0 件とみなす）。
//
// **日が明ける前（その日の 23:59 まで）にしか取れていない日は済みにしない。** 当日ぶんは
// 23:59 まで配信が続くので、夜の配信が抜けたままになる。朝の --days 5 が取り直して埋める
// （鮮度の判定 Fresh も同じ基準で「揃っていない」と見る）。
func (s *Store) DoneDays(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT feed_date, fetched_at FROM news_days WHERE status = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var day, stamp string
		if err := rows.Scan(&day, &stamp); err != nil {
			return nil, err
		}
		complete, err := completeDay(day, stamp)
		if err != nil {
			return nil, err
		}
		out[day] = complete
	}
	return out, rows.Err()
}

// completeDay は feed_date の日が、日が明けてから（翌日 0:00 JST 以降に）取れているか。
func completeDay(day, fetchedAt string) (bool, error) {
	at, err := time.Parse(time.RFC3339, fetchedAt)
	if err != nil {
		return false, fmt.Errorf("fetched_at を読めません（%s %s）: %w", day, fetchedAt, err)
	}
	end, err := dayEnd(day)
	if err != nil {
		return false, err
	}
	return !at.Before(end), nil
}

// dayEnd は feed_date（YYYY-MM-DD）の日が明ける時刻（翌日 0:00 JST）。
func dayEnd(day string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", day, clock.Tokyo)
	if err != nil {
		return time.Time{}, fmt.Errorf("feed_date を読めません（%s）: %w", day, err)
	}
	return d.AddDate(0, 0, 1), nil
}

// Coverage は溜まり具合（日数・記事数・最初と最後の日）を返す。
func (s *Store) Coverage(ctx context.Context) (days, items int, first, last string, err error) {
	err = s.db.QueryRowContext(ctx, `
        SELECT COUNT(*), COALESCE(SUM(items), 0), COALESCE(MIN(feed_date), ''), COALESCE(MAX(feed_date), '')
        FROM news_days WHERE status = 'ok'`).Scan(&days, &items, &first, &last)
	return
}
