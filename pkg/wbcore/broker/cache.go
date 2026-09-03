package broker

import (
	"sync"
	"time"
)

// Cached は短時間だけ取得結果を使い回す。
//
// 残高や建玉のように「2 回 / 2 秒」しか叩けないものを、1 回の実行サイクル内で
// 何度も参照したい場合に使う。
//
//	balance := NewCached(broker.FetchBalance, 5*time.Second)
//	balance.Get() // 1回目は実際に取得
//	balance.Get() // 5秒以内は同じ値を返す
//
// goroutine 安全。
type Cached[T any] struct {
	factory func() (T, error)
	ttl     time.Duration
	now     func() time.Time

	mu        sync.Mutex
	value     T
	fetchedAt time.Time
	valid     bool
}

// DefaultCacheTTL は既定の保持時間。証券会社の照会系の上限（2 回/2 秒）に
// ぶつからない程度の短さで、同一サイクル内の重複照会は潰せる長さ。
const DefaultCacheTTL = 2 * time.Second

// NewCached は取得関数と保持時間からキャッシュを作る。ttl が 0 以下なら既定値。
func NewCached[T any](factory func() (T, error), ttl time.Duration) *Cached[T] {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cached[T]{factory: factory, ttl: ttl, now: time.Now}
}

// SetClock は時刻の取り方を差し替える（テスト用）。
func (c *Cached[T]) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now != nil {
		c.now = now
	}
}

// Get は保持期間内なら前回の値を、切れていれば取り直して返す。
//
// 取得に失敗した値はキャッシュしない。エラーを ttl のあいだ持ち回ると、
// 一時的な通信断で実行サイクル全体が使えなくなるため。
func (c *Cached[T]) Get() (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.valid && now.Sub(c.fetchedAt) < c.ttl {
		return c.value, nil
	}
	value, err := c.factory()
	if err != nil {
		var zero T
		return zero, err
	}
	c.value = value
	c.fetchedAt = now
	c.valid = true
	return value, nil
}

// Invalidate は次回の取得を強制する。発注直後など、状態が変わったときに呼ぶ。
func (c *Cached[T]) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	c.value = zero
	c.valid = false
	c.fetchedAt = time.Time{}
}
