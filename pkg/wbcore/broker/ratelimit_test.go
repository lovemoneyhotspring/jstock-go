package broker

import (
	"errors"
	"testing"
	"time"
)

func TestLimitValidation(t *testing.T) {
	if _, err := NewLimit(0, 1); err == nil {
		t.Error("0 回/秒 は不正")
	}
	if _, err := NewLimit(2, 0); err == nil {
		t.Error("0 秒あたり は不正")
	}
	limit, err := NewLimit(2, 2)
	if err != nil || limit.String() != "2回/2秒" {
		t.Fatalf("limit = %v, %v", limit, err)
	}
}

func TestAcquireWaitsWhenExhausted(t *testing.T) {
	limiter := NewRateLimiter(Limit{Calls: 2, PerSeconds: 2})
	var slept time.Duration
	// 実際に眠らせるとテストが遅くなるだけなので、待ち時間だけ観測する
	limiter.SetSleep(func(d time.Duration) { slept += d })

	// バケットは満杯から始まるので、最初の 2 回は待たない
	for i := 0; i < 2; i++ {
		waited, err := limiter.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if waited != 0 {
			t.Fatalf("%d 回目で待った: %v", i+1, waited)
		}
	}
	// 3 回目は回復待ち（2 回/2 秒 = 1 秒に 1 回）
	waited, err := limiter.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if waited <= 0 || waited > 1100*time.Millisecond {
		t.Fatalf("待ち時間 = %v", waited)
	}
	if slept != waited {
		t.Errorf("待った時間と返り値が食い違う: %v vs %v", slept, waited)
	}
}

func TestAcquireRejectsOversizedRequest(t *testing.T) {
	limiter := NewRateLimiter(Limit{Calls: 2, PerSeconds: 2})
	if _, err := limiter.AcquireN(3); err == nil {
		t.Fatal("上限を超える要求は待っても満たせないので弾くべき")
	}
	if waited, err := limiter.AcquireN(0); err != nil || waited != 0 {
		t.Fatalf("0 個の要求 = %v, %v", waited, err)
	}
}

func TestCachedReusesWithinTTL(t *testing.T) {
	calls := 0
	now := time.Unix(0, 0)
	cache := NewCached(func() (int, error) {
		calls++
		return calls, nil
	}, 2*time.Second)
	cache.SetClock(func() time.Time { return now })

	if v, _ := cache.Get(); v != 1 || calls != 1 {
		t.Fatalf("1 回目 = %d（呼び出し %d）", v, calls)
	}
	now = now.Add(time.Second)
	if v, _ := cache.Get(); v != 1 || calls != 1 {
		t.Fatalf("TTL 内で取り直している: %d（呼び出し %d）", v, calls)
	}
	now = now.Add(2 * time.Second)
	if v, _ := cache.Get(); v != 2 || calls != 2 {
		t.Fatalf("TTL 切れで取り直していない: %d（呼び出し %d）", v, calls)
	}
}

func TestCachedInvalidate(t *testing.T) {
	calls := 0
	cache := NewCached(func() (int, error) {
		calls++
		return calls, nil
	}, time.Hour)
	if _, err := cache.Get(); err != nil {
		t.Fatal(err)
	}
	// 発注直後など、状態が変わったら取り直す
	cache.Invalidate()
	if v, _ := cache.Get(); v != 2 {
		t.Fatalf("Invalidate 後 = %d", v)
	}
}

func TestCachedDoesNotCacheErrors(t *testing.T) {
	boom := errors.New("通信断")
	calls := 0
	cache := NewCached(func() (int, error) {
		calls++
		if calls == 1 {
			return 0, boom
		}
		return 42, nil
	}, time.Hour)

	if _, err := cache.Get(); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// 一時的な通信断を TTL のあいだ持ち回ると、実行サイクル全体が使えなくなる
	if v, err := cache.Get(); err != nil || v != 42 {
		t.Fatalf("再試行 = %d, %v", v, err)
	}
}
