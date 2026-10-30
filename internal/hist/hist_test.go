package hist

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

const hour = int64(time.Hour)

func nearestRank(sorted []int64, q float64) int64 {
	if q <= 0 {
		return sorted[0]
	}
	rank := int(math.Ceil(q*float64(len(sorted)) - 1e-9))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, d := range []int{0, 6, -1} {
		if _, err := New(d, hour); err == nil {
			t.Errorf("digits=%d: expected error", d)
		}
	}
	if _, err := New(3, 0); err == nil {
		t.Error("highest=0: expected error")
	}
}

func TestEmptyHistogram(t *testing.T) {
	h := MustNew(3, hour)
	if h.Count() != 0 || h.Min() != 0 || h.Max() != 0 || h.Mean() != 0 || h.ValueAtQuantile(0.99) != 0 {
		t.Fatal("empty histogram should report zeros")
	}
}

func TestExactInLinearRange(t *testing.T) {
	h := MustNew(3, hour)
	// p = 11 for 3 digits, so every value below 2048 has its own bucket.
	for v := int64(0); v < 2048; v++ {
		h.Record(v)
	}
	for _, q := range []float64{0.001, 0.25, 0.5, 0.9, 0.999} {
		want := int64(math.Ceil(q*2048-1e-9)) - 1
		if got := h.ValueAtQuantile(q); got != want {
			t.Errorf("q=%v: got %d want %d", q, got, want)
		}
	}
}

func TestBucketBoundsContainValue(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for digits := 1; digits <= 4; digits++ {
		h := MustNew(digits, hour)
		for range 20000 {
			v := int64(math.Exp(rng.Float64() * math.Log(float64(hour))))
			i := h.index(v)
			lo, hi := h.bounds(i)
			if v < lo || v > hi {
				t.Fatalf("digits=%d v=%d not in bucket %d [%d,%d]", digits, v, i, lo, hi)
			}
			if h.index(lo) != i || h.index(hi) != i {
				t.Fatalf("digits=%d bucket %d edges map elsewhere", digits, i)
			}
			if lo > 0 && float64(hi-lo)/float64(lo) > math.Pow(10, -float64(digits)) {
				t.Fatalf("digits=%d bucket [%d,%d] wider than relative bound", digits, lo, hi)
			}
		}
	}
}

func TestIndexIsMonotonic(t *testing.T) {
	h := MustNew(2, 1<<20)
	prev := -1
	for v := int64(0); v <= 1<<20; v++ {
		i := h.index(v)
		if i < prev || i > prev+1 {
			t.Fatalf("index jumped from %d to %d at v=%d", prev, i, v)
		}
		prev = i
	}
	if prev != h.Buckets()-1 {
		t.Fatalf("last index %d, buckets %d", prev, h.Buckets())
	}
}

// TestPercentileAccuracyBounds is a property test: for several latency-like
// distributions the histogram's percentiles must never under-report the exact
// nearest-rank percentile and must over-report by at most 10^-digits.
func TestPercentileAccuracyBounds(t *testing.T) {
	ms := float64(time.Millisecond)
	dists := map[string]func(r *rand.Rand) int64{
		"uniform_0_1s":   func(r *rand.Rand) int64 { return r.Int64N(int64(time.Second)) },
		"exponential":    func(r *rand.Rand) int64 { return int64(r.ExpFloat64() * 20 * ms) },
		"lognormal":      func(r *rand.Rand) int64 { return int64(math.Exp(r.NormFloat64()*1.2) * 5 * ms) },
		"bimodal_stalls": bimodal,
		"tiny_values":    func(r *rand.Rand) int64 { return r.Int64N(5000) },
	}
	quantiles := []float64{0, 0.5, 0.75, 0.9, 0.99, 0.999, 0.9999, 1}
	for name, gen := range dists {
		for digits := 1; digits <= 4; digits++ {
			rng := rand.New(rand.NewPCG(uint64(digits), 42))
			h := MustNew(digits, hour)
			vals := make([]int64, 50000)
			for i := range vals {
				vals[i] = gen(rng)
				h.Record(vals[i])
			}
			slices.Sort(vals)
			bound := math.Pow(10, -float64(digits))
			for _, q := range quantiles {
				exact := nearestRank(vals, q)
				got := h.ValueAtQuantile(q)
				if got < exact {
					t.Errorf("%s digits=%d q=%v: under-reported %d < exact %d", name, digits, q, got, exact)
				}
				if exact > 0 && float64(got-exact)/float64(exact) > bound {
					t.Errorf("%s digits=%d q=%v: got %d exact %d, rel err %.6f > %.6f",
						name, digits, q, got, exact, float64(got-exact)/float64(exact), bound)
				}
			}
			if h.Min() != vals[0] || h.Max() != vals[len(vals)-1] {
				t.Errorf("%s: min/max %d/%d, want %d/%d", name, h.Min(), h.Max(), vals[0], vals[len(vals)-1])
			}
			var sum float64
			for _, v := range vals {
				sum += float64(v)
			}
			if mean := sum / float64(len(vals)); math.Abs(h.Mean()-mean) > 1e-6*mean {
				t.Errorf("%s: mean %.3f want %.3f", name, h.Mean(), mean)
			}
		}
	}
}

func bimodal(r *rand.Rand) int64 {
	if r.IntN(100) == 0 {
		return int64(time.Second) + r.Int64N(int64(time.Second))
	}
	return int64(time.Millisecond) + r.Int64N(int64(2*time.Millisecond))
}

func TestMergeEqualsSingleHistogram(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	all, a, b := MustNew(3, hour), MustNew(3, hour), MustNew(3, hour)
	for i := range 30000 {
		v := int64(rng.ExpFloat64() * float64(time.Millisecond))
		all.Record(v)
		if i%3 == 0 {
			a.Record(v)
		} else {
			b.Record(v)
		}
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Count() != all.Count() || a.Min() != all.Min() || a.Max() != all.Max() {
		t.Fatalf("merged summary differs")
	}
	if !slices.Equal(a.counts, all.counts) {
		t.Fatal("merged counts differ")
	}
	if err := a.Merge(MustNew(2, hour)); err == nil {
		t.Fatal("merging different configurations must fail")
	}
}

func TestSaturationClampsToHighest(t *testing.T) {
	h := MustNew(2, 1000)
	h.Record(5)
	h.Record(5000)
	h.Record(-3)
	if h.Saturated() != 1 || h.Max() != 1000 || h.Min() != 0 {
		t.Fatalf("saturated=%d max=%d min=%d", h.Saturated(), h.Max(), h.Min())
	}
	if h.ValueAtQuantile(1) != 1000 {
		t.Fatalf("p100=%d", h.ValueAtQuantile(1))
	}
}

func TestRelativeErrorWithinDigits(t *testing.T) {
	for d := 1; d <= 5; d++ {
		h := MustNew(d, 1<<20)
		if re := h.RelativeError(); re > math.Pow(10, -float64(d)) {
			t.Errorf("digits=%d relative error %g", d, re)
		}
	}
}

func BenchmarkRecord(b *testing.B) {
	h := MustNew(3, hour)
	rng := rand.New(rand.NewPCG(1, 1))
	vals := make([]int64, 4096)
	for i := range vals {
		vals[i] = int64(rng.ExpFloat64() * float64(time.Millisecond))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Record(vals[i&4095])
	}
}
