// Package hist implements a log-linear ("HDR-style") histogram for latency values.
//
// Values below 2^p are counted exactly. Every power-of-two range [2^k, 2^(k+1))
// above that is split into 2^(p-1) equal-width sub-buckets, so a bucket is never
// wider than 2^-(p-1) of the smallest value it holds. p is derived from the
// requested number of significant decimal digits, which bounds the relative error
// of every reported percentile by 10^-digits. Memory is O(log2(range) * 2^p) and
// does not grow with the number of recorded values.
package hist

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Histogram counts non-negative int64 values (loadgen records nanoseconds).
// It is not safe for concurrent use; callers must synchronise.
type Histogram struct {
	digits    int
	subBits   uint  // p: values < 2^p are exact
	half      int64 // 2^(p-1): sub-buckets per power of two above the exact range
	highest   int64
	counts    []uint64
	total     uint64
	saturated uint64
	min       int64
	max       int64
	sum       float64
}

// New returns a histogram that tracks values in [0, highest] with the given
// number of significant decimal digits (1..5). Larger values are clamped to
// highest and counted as saturated.
func New(digits int, highest int64) (*Histogram, error) {
	if digits < 1 || digits > 5 {
		return nil, fmt.Errorf("hist: significant digits must be in 1..5, got %d", digits)
	}
	if highest < 1 {
		return nil, fmt.Errorf("hist: highest trackable value must be >= 1, got %d", highest)
	}
	p := uint(math.Ceil(float64(digits)*math.Log2(10))) + 1
	h := &Histogram{
		digits:  digits,
		subBits: p,
		half:    int64(1) << (p - 1),
		highest: highest,
	}
	h.counts = make([]uint64, h.index(highest)+1)
	h.Reset()
	return h, nil
}

// MustNew is New for configurations known to be valid.
func MustNew(digits int, highest int64) *Histogram {
	h, err := New(digits, highest)
	if err != nil {
		panic(err)
	}
	return h
}

// index maps a value to its bucket. Below 2^p the mapping is the identity; above
// it, the value's top p bits select a sub-bucket within its power-of-two range.
func (h *Histogram) index(v int64) int {
	u := uint64(v)
	l := uint(bits.Len64(u))
	if l <= h.subBits {
		return int(v)
	}
	shift := l - h.subBits
	return int(int64(shift)*h.half + int64(u>>shift))
}

// bounds returns the smallest and largest value that map to bucket i.
func (h *Histogram) bounds(i int) (lo, hi int64) {
	idx := int64(i)
	if idx < 2*h.half {
		return idx, idx
	}
	shift := idx/h.half - 1
	top := idx - shift*h.half
	lo = top << shift
	return lo, lo + (int64(1) << shift) - 1
}

// Record adds one value.
func (h *Histogram) Record(v int64) { h.RecordN(v, 1) }

// RecordN adds n occurrences of v. Negative values are recorded as 0.
func (h *Histogram) RecordN(v int64, n uint64) {
	if n == 0 {
		return
	}
	if v < 0 {
		v = 0
	}
	if v > h.highest {
		v = h.highest
		h.saturated += n
	}
	h.counts[h.index(v)] += n
	h.total += n
	h.sum += float64(v) * float64(n)
	if v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
}

// ValueAtQuantile returns the value at quantile q in [0, 1] using the
// nearest-rank definition. The result is the upper edge of the bucket holding
// that rank (clamped to the observed max), so it never under-reports and
// over-reports by at most RelativeError.
func (h *Histogram) ValueAtQuantile(q float64) int64 {
	if h.total == 0 {
		return 0
	}
	if q <= 0 {
		return h.min
	}
	if q >= 1 {
		return h.max
	}
	rank := uint64(math.Ceil(q*float64(h.total) - 1e-9))
	if rank == 0 {
		rank = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= rank {
			_, hi := h.bounds(i)
			return min(max(hi, h.min), h.max)
		}
	}
	return h.max
}

// Merge adds all counts of o into h. Both must share digits and highest.
func (h *Histogram) Merge(o *Histogram) error {
	if o.digits != h.digits || o.highest != h.highest {
		return errors.New("hist: cannot merge histograms with different configurations")
	}
	if o.total == 0 {
		return nil
	}
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.total += o.total
	h.saturated += o.saturated
	h.sum += o.sum
	h.min = min(h.min, o.min)
	h.max = max(h.max, o.max)
	return nil
}

// Reset clears all counts.
func (h *Histogram) Reset() {
	clear(h.counts)
	h.total, h.saturated, h.sum = 0, 0, 0
	h.min, h.max = math.MaxInt64, 0
}

// Count is the number of recorded values.
func (h *Histogram) Count() uint64 { return h.total }

// Saturated is the number of values that exceeded the trackable range.
func (h *Histogram) Saturated() uint64 { return h.saturated }

// Min is the smallest recorded value (0 when empty).
func (h *Histogram) Min() int64 {
	if h.total == 0 {
		return 0
	}
	return h.min
}

// Max is the largest recorded value.
func (h *Histogram) Max() int64 { return h.max }

// Mean is the exact arithmetic mean of the recorded (clamped) values.
func (h *Histogram) Mean() float64 {
	if h.total == 0 {
		return 0
	}
	return h.sum / float64(h.total)
}

// Digits is the configured number of significant decimal digits.
func (h *Histogram) Digits() int { return h.digits }

// Highest is the largest trackable value.
func (h *Histogram) Highest() int64 { return h.highest }

// RelativeError is the worst-case relative over-estimate of ValueAtQuantile.
func (h *Histogram) RelativeError() float64 { return math.Ldexp(1, -int(h.subBits-1)) }

// Buckets is the number of counters allocated (a proxy for memory use).
func (h *Histogram) Buckets() int { return len(h.counts) }
