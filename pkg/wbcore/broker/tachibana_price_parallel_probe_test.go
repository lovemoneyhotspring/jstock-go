package broker

// 時価問合（CLMMfdsGetMarketPrice）を並列に送れるかを実地で確かめる調べもの。注文は出さない。
//
// **場の時間外・cron の発注系と重ならない時刻に走らせる。** 実験のあいだ（10 秒以上）本番のセッションの flock を
// 握り続ける。flock の待ちは締め切りを見ないので、同じ時刻の daytrade open / close がそのぶん止まる。
//
// postTo は「採番 → 保存 → 送信 → 応答」を 1 つのロックで直列にしている。気配 8 本（914 銘柄）の
// 1.5 秒を縮めるには並列に送りたいが、p_no（通番）が逆転して届いたときに立花が何を返すかを知らない。
// 未知の p_errno はセッション失効として扱われ、9:00 の発注の直前に再ログインが入りうる。
//
//	A: 直列で 1 本（基準の所要）
//	B: 8 本を同時に送る（p_no は昇順に採番。到着順は保証されない）
//	C: わざと逆順——大きい p_no の応答を待ってから、小さい p_no を送る
//	D: 応答を待たずに、番号順に一定の間隔でずらして 8 本送る（TACHIBANA_PROBE_STAGGER_MS=50,30,20,10 を各 3 回）
//
//	TACHIBANA_PRICE_PARALLEL_PROBE=1 TACHIBANA_PROBE_SYMBOLS=7203,6758,... \
//	  go test ./pkg/wbcore/broker -run TestPriceParallelProbe -v -count=1
import (
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPriceParallelProbe(t *testing.T) {
	if os.Getenv("TACHIBANA_PRICE_PARALLEL_PROBE") == "" {
		t.Skip("TACHIBANA_PRICE_PARALLEL_PROBE=1 を立てたときだけ動かす")
	}
	var symbols []string
	for _, s := range strings.Split(os.Getenv("TACHIBANA_PROBE_SYMBOLS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			symbols = append(symbols, s)
		}
	}
	if len(symbols) == 0 {
		symbols = []string{"7203", "6758", "9984", "8306", "6861"}
	}
	b := newProbeBroker(t)

	// 実験のあいだセッションを握り、番号をまとめて確保して先に書く（他のプロセスはこの上から採番する）
	b.mu.Lock()
	defer b.mu.Unlock()
	path := b.sessionFilePath()
	unlock, err := lockSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := b.ensureSessionLocked(path); err != nil {
		t.Fatal(err)
	}
	base := b.session.PNo
	b.session.PNo += 400
	if err := writeSessionFile(path, b.session); err != nil {
		t.Fatal(err)
	}
	t.Logf("p_no %d〜%d を確保。銘柄 %d", base, base+399, len(symbols))

	type result struct {
		pNo         int
		errno, perr string
		rows        int
		sent, took  time.Duration
		err         error
	}
	start := time.Now()
	ask := func(pNo int, batch []string) result {
		sent := time.Since(start)
		res, err := b.send(interfacePrice, pNo, clmMarketPrice, map[string]any{
			"sTargetIssueCode": strings.Join(batch, ","), "sTargetColumn": MarketPriceColumns,
		})
		r := result{pNo: pNo, sent: sent, took: time.Since(start) - sent, err: err}
		if err == nil {
			r.errno, r.perr = strings.TrimSpace(text(res["p_errno"])), strings.TrimSpace(text(res["p_err"]))
			if rows, e := rowsOf(res, marketPriceKey, clmMarketPrice); e == nil {
				r.rows = len(rows)
			}
		}
		return r
	}
	show := func(label string, r result) {
		t.Logf("%s p_no=%d sent=+%dms took=%dms errno=%q err=%q rows=%d goerr=%v",
			label, r.pNo, r.sent.Milliseconds(), r.took.Milliseconds(), r.errno, r.perr, r.rows, r.err)
	}
	batchOf := func(i int) []string {
		lo := (i * MarketPriceBatch) % len(symbols)
		return symbols[lo:min(lo+MarketPriceBatch, len(symbols))]
	}

	show("A 直列", ask(base, batchOf(0)))

	const n = 8
	out := make([]result, n)
	var wg sync.WaitGroup
	began := time.Since(start)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = ask(base+1+i, batchOf(i))
		}()
	}
	wg.Wait()
	for _, r := range out {
		show("B 並列", r)
	}
	t.Logf("B 並列 8 本の合計 %dms", (time.Since(start) - began).Milliseconds())

	next := base + 30
	for _, f := range strings.Split(os.Getenv("TACHIBANA_PROBE_STAGGER_MS"), ",") {
		ms, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			continue
		}
		for trial := 1; trial <= 3; trial++ {
			res := make([]result, n)
			var g sync.WaitGroup
			t0 := time.Since(start)
			for i := range n {
				g.Add(1)
				pNo := next
				next++
				go func() {
					defer g.Done()
					res[i] = ask(pNo, batchOf(i))
				}()
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			g.Wait()
			okCount, rows := 0, 0
			for _, r := range res {
				if r.errno == "0" && r.err == nil {
					okCount++
					rows += r.rows
				} else {
					show("D 失敗", r)
				}
			}
			t.Logf("D 間隔 %dms 試行 %d: 成功 %d/%d、%d 行、合計 %dms", ms, trial, okCount, n, rows, (time.Since(start) - t0).Milliseconds())
			time.Sleep(300 * time.Millisecond)
		}
	}

	show("C 逆順 1（大）", ask(base+391, batchOf(0)))
	show("C 逆順 2（小）", ask(base+390, batchOf(1)))
	show("C 逆順のあと（さらに大）", ask(base+392, batchOf(0)))
}

// 実装した経路（MarketPricesRawPartialAt）を実機で通す。ずらして送る形と直列（TACHIBANA_PRICE_STAGGER_MS=0）を
// 交互に 3 回ずつ。見るのは所要・行数・失敗したバッチ・応答を受けた時刻の幅。
//
//	TACHIBANA_PRICE_PIPELINED_PROBE=1 TACHIBANA_PROBE_SYMBOLS=... go test ./pkg/wbcore/broker -run TestPricePipelinedProbe -v -count=1
func TestPricePipelinedProbe(t *testing.T) {
	if os.Getenv("TACHIBANA_PRICE_PIPELINED_PROBE") == "" {
		t.Skip("TACHIBANA_PRICE_PIPELINED_PROBE=1 を立てたときだけ動かす")
	}
	var symbols []string
	for _, s := range strings.Split(os.Getenv("TACHIBANA_PROBE_SYMBOLS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			symbols = append(symbols, s)
		}
	}
	b := newProbeBroker(t)
	for trial := 1; trial <= 3; trial++ {
		for _, ms := range []string{"", "0"} {
			t.Setenv("TACHIBANA_PRICE_STAGGER_MS", ms)
			start := time.Now()
			rows, at, failed := b.MarketPricesRawPartialAt(symbols, "")
			took := time.Since(start)
			var spread time.Duration
			if len(at) > 0 {
				lo, hi := at[0], at[0]
				for _, x := range at {
					if x.Before(lo) {
						lo = x
					}
					if x.After(hi) {
						hi = x
					}
				}
				spread = hi.Sub(lo)
			}
			t.Logf("試行 %d 間隔 %q: %d 行、失敗 %d バッチ、所要 %dms、応答の時刻の幅 %dms", trial, ms, len(rows), len(failed), took.Milliseconds(), spread.Milliseconds())
			for _, f := range failed {
				t.Logf("  失敗: %v", f)
			}
			time.Sleep(1200 * time.Millisecond)
		}
	}
}
