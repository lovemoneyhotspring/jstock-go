package broker

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"
)

// Limit は per_seconds 秒あたり calls 回までの上限。
//
// 証券会社の API には「残高照会は 2 回 / 2 秒」のような上限がある。銘柄ごとに
// 残高を確認するような実装にすると即座に上限に当たるので、呼び出し側でまとめて
// 取得し、Cached で短時間だけ使い回す。上限そのものは、それを知っている
// Broker の実装が Limit で宣言する。
type Limit struct {
	Calls      int
	PerSeconds float64
}

// NewLimit は上限を検証して作る。
func NewLimit(calls int, perSeconds float64) (Limit, error) {
	limit := Limit{Calls: calls, PerSeconds: perSeconds}
	if err := limit.Validate(); err != nil {
		return Limit{}, err
	}
	return limit, nil
}

func (l Limit) Validate() error {
	if l.Calls < 1 || l.PerSeconds <= 0 {
		return fmt.Errorf("不正なレート制限: %d回/%g秒", l.Calls, l.PerSeconds)
	}
	return nil
}

func (l Limit) String() string {
	return fmt.Sprintf("%d回/%g秒", l.Calls, l.PerSeconds)
}

// RateLimiter はトークンバケット方式のレート制限。
//
// 上限に達したら例外ではなく待つ。自動売買では「制限に当たったので発注しません
// でした」より「少し待って発注する」方が望ましいため。
//
// 実体は準標準ライブラリの golang.org/x/time/rate。バケットの実装を自前で持つと
// 時刻の扱いを間違えたときに静かに上限を超えるので、枯れたものに任せる。
// goroutine 安全。
type RateLimiter struct {
	limit   Limit
	limiter *rate.Limiter
	// sleep はテストが時間を進めずに待ち時間を観測するための差し替え口。
	sleep func(time.Duration)
}

// NewRateLimiter は上限からレート制限を作る。上限が不正なら panic する
// （設定ミスであり、実行時に握り潰すと上限超過で API を止められるため）。
func NewRateLimiter(limit Limit) *RateLimiter {
	if err := limit.Validate(); err != nil {
		panic(err)
	}
	return &RateLimiter{
		limit:   limit,
		limiter: rate.NewLimiter(rate.Limit(float64(limit.Calls)/limit.PerSeconds), limit.Calls),
		sleep:   time.Sleep,
	}
}

// SetSleep は待ち方を差し替える（テスト用）。
func (r *RateLimiter) SetSleep(sleep func(time.Duration)) {
	if sleep != nil {
		r.sleep = sleep
	}
}

// Limit は宣言された上限。
func (r *RateLimiter) Limit() Limit { return r.limit }

// Acquire はトークンを 1 つ消費する。足りなければ回復するまで待ち、
// 実際に待った時間を返す。
func (r *RateLimiter) Acquire() (time.Duration, error) {
	return r.AcquireN(1)
}

// AcquireN はトークンを tokens 個消費する。
func (r *RateLimiter) AcquireN(tokens int) (time.Duration, error) {
	if tokens > r.limit.Calls {
		return 0, fmt.Errorf("1回の要求 %d が上限 %d を超えています", tokens, r.limit.Calls)
	}
	if tokens <= 0 {
		return 0, nil
	}
	reservation := r.limiter.ReserveN(time.Now(), tokens)
	if !reservation.OK() {
		return 0, fmt.Errorf("レート制限 %s ではトークン %d を取得できません", r.limit, tokens)
	}
	delay := reservation.Delay()
	if delay > 0 {
		r.sleep(delay)
	}
	return delay, nil
}

// Wait は context の期限を尊重して 1 トークン待つ。
// 長時間の待ちが起こりうる経路（同期処理など）では、こちらで中断できるようにする。
func (r *RateLimiter) Wait(ctx context.Context) error {
	return r.limiter.Wait(ctx)
}

func (r *RateLimiter) String() string {
	return "<RateLimiter " + r.limit.String() + ">"
}
