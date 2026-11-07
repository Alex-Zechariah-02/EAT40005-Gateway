// Tests of the rate primitives: each limiter's ceiling and guarantee, found by simulation, and the conversions.

package semantics

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

const second = 1000 // one window of one second, in milliseconds

// run sends, at each listed time, requests until one is refused or the
// listed count is sent, and returns the times of admitted requests.
func run(limiter Limiter, times []int64, counts []int) []int64 {
	var admitted []int64
	for i, now := range times {
		for range counts[i] {
			if !limiter.Admit(now) {
				break
			}
			admitted = append(admitted, now)
		}
	}
	return admitted
}

// busiest is the most admitted times in any interval [t, t+window).
func busiest(times []int64, window int64) int {
	most, start := 0, 0
	for end := range times {
		for times[end]-times[start] >= window {
			start++
		}
		most = max(most, end-start+1)
	}
	return most
}

// greedy asks for many requests at every millisecond from offset on.
func greedy(offset int64) ([]int64, []int) {
	var times []int64
	var counts []int
	for now := offset; now < offset+3*second; now++ {
		times, counts = append(times, now), append(counts, 1<<20)
	}
	return times, counts
}

// random sends random bursts at random, increasing times.
func random(r *rand.Rand) ([]int64, []int) {
	var times []int64
	var counts []int
	for now := r.Int64N(second); now < 4*second; now += 1 + r.Int64N(second/8) {
		times, counts = append(times, now), append(counts, 1+r.IntN(8))
	}
	return times, counts
}

// conforming sends bursts of g, one each window, at the given phase.
func conforming(g int, phase int64) ([]int64, []int) {
	var times []int64
	var counts []int
	for now := phase; now < 40*second; now += second {
		times, counts = append(times, now), append(counts, g)
	}
	return times, counts
}

// checkBounds proves a rate's Ceiling is reached and never exceeded, and
// that its Guarantee is always admitted while one more is not.
func checkBounds(t *testing.T, rate Rate) {
	t.Helper()
	ceiling, guarantee := rate.Ceiling(), rate.Guarantee()
	reached := 0
	r := rand.New(rand.NewPCG(uint64(rate.Count), uint64(rate.Burst)))
	check := func(times []int64, counts []int) {
		n := busiest(run(rate.Limiter(), times, counts), second)
		if n > int(ceiling) {
			t.Fatalf("%+v admitted %d in one window, ceiling %d", rate, n, ceiling)
		}
		reached = max(reached, n)
	}
	for _, offset := range []int64{0, 1, second / 2, second - int64(rate.Count), second - 2, second - 1} {
		check(greedy(max(offset, 0)))
	}
	for range 300 {
		check(random(r))
	}
	if reached != int(ceiling) {
		t.Fatalf("%+v: most admitted %d, ceiling %d", rate, reached, ceiling)
	}
	refused := false
	for phase := int64(0); phase < second; phase += 7 {
		times, counts := conforming(int(guarantee), phase)
		if len(run(rate.Limiter(), times, counts)) != len(times)*int(guarantee) {
			t.Fatalf("%+v refused a sender within the guarantee %d at phase %d", rate, guarantee, phase)
		}
		times, counts = conforming(int(guarantee)+1, phase)
		refused = refused || len(run(rate.Limiter(), times, counts)) < len(times)*(int(guarantee)+1)
	}
	if !refused {
		t.Fatalf("%+v admitted one more than the guarantee %d at every phase", rate, guarantee)
	}
}

// TestRateBounds simulates the gateway's own weighted sliding window: at most
// 2N-1 in any window, and every burst of N/2 rounded down admitted.
func TestRateBounds(t *testing.T) {
	for _, n := range []int64{1, 2, 3, 5, 10, 60} {
		rate := Rate{Algorithm: WeightedSliding, Count: n, Window: 1}
		if rate.Ceiling() != 2*n-1 || rate.Guarantee() != n/2 {
			t.Fatalf("N=%d: ceiling %d, guarantee %d", n, rate.Ceiling(), rate.Guarantee())
		}
		checkBounds(t, rate)
	}
}

// TestOtherRateBounds checks every other algorithm's stated bounds the same way.
func TestOtherRateBounds(t *testing.T) {
	for _, n := range []int64{1, 2, 5, 10} {
		for _, rate := range []Rate{
			{Algorithm: FixedAligned, Count: n, Window: 1},
			{Algorithm: FixedFirstRequest, Count: n, Window: 1},
			{Algorithm: SlidingLog, Count: n, Window: 1},
			{Algorithm: TokenBucket, Count: n, Window: 1, Burst: n},
			{Algorithm: TokenBucket, Count: n, Window: 1, Burst: 3 * n},
			{Algorithm: LeakyBucket, Count: n, Window: 1, Burst: n - 1},
			{Algorithm: LeakyBucket, Count: n, Window: 1, Burst: 2 * n},
		} {
			checkBounds(t, rate)
		}
	}
}

func TestRateConvert(t *testing.T) {
	cases := []struct {
		rate         Rate
		count        int64
		class        exchange.Class
		direction    exchange.Direction
		noteContains string
	}{
		{Rate{Algorithm: WeightedSliding, Count: 10, Window: 60}, 10, exchange.Exact, "", ""},
		{Rate{Algorithm: FixedAligned, Count: 10, Window: 60}, 10, exchange.Approximated, exchange.Both, "ceiling 19 ≤ 20"},
		{Rate{Algorithm: FixedFirstRequest, Count: 10, Window: 60}, 10, exchange.Approximated, exchange.Both, "guarantee 5 < 10"},
		{Rate{Algorithm: SlidingLog, Count: 10, Window: 60}, 5, exchange.Approximated, exchange.Both, "ceiling 9 ≤ 10"},
		{Rate{Algorithm: TokenBucket, Count: 10, Window: 60, Burst: 4}, 7, exchange.Approximated, exchange.Both, "ceiling 13 ≤ 13"},
		{Rate{Algorithm: TokenBucket, Count: 10, Window: 60, Burst: 40}, 10, exchange.Approximated, exchange.Both, ""},
		{Rate{Algorithm: LeakyBucket, Count: 10, Window: 60, Burst: 9, Delay: true}, 0, exchange.NotExpressible, "", "delays"},
		{Rate{Algorithm: FixedAligned, Count: 10, Window: 2592000}, 0, exchange.NotExpressible, "", "86400"},
		{Rate{Algorithm: FixedAligned, Count: 0, Window: 60}, 0, exchange.NotExpressible, "", "1 to 1000000"},
	}
	for _, c := range cases {
		count, window, distance := c.rate.Convert()
		if count != c.count || distance.Class != c.class || distance.Direction != c.direction ||
			!strings.Contains(distance.Note, c.noteContains) {
			t.Errorf("%+v: got %d/%d %+v", c.rate, count, window, distance)
		}
		if count > 0 && window != c.rate.Window {
			t.Errorf("%+v: window %d", c.rate, window)
		}
	}
}

func TestKeyConvert(t *testing.T) {
	for key, want := range map[Key]string{KeyCaller: "subject", KeyClientAddress: "client_address", KeyShared: "", KeyHeader: "", KeyPath: ""} {
		got, distance := key.Convert()
		if got != want || (want == "") != (distance.Class == exchange.NotExpressible) {
			t.Errorf("%s: %q %+v", key, got, distance)
		}
	}
}
