package schedule

import (
	"math"
	"testing"
	"time"
)

func collect(t *testing.T, p *Profile) []time.Duration {
	t.Helper()
	var out []time.Duration
	a := p.Arrivals()
	for {
		off, ok := a.Next()
		if !ok {
			break
		}
		out = append(out, off)
	}
	if int64(len(out)) != p.Count() {
		t.Fatalf("Arrivals yielded %d, Count() says %d", len(out), p.Count())
	}
	for i := 1; i < len(out); i++ {
		if out[i] < out[i-1] {
			t.Fatalf("offsets not monotonic at %d: %v < %v", i, out[i], out[i-1])
		}
	}
	if len(out) > 0 && out[len(out)-1] >= p.Duration() {
		t.Fatalf("last offset %v not inside profile duration %v", out[len(out)-1], p.Duration())
	}
	return out
}

// perSecond counts offsets that fall into each whole-second window.
func perSecond(offs []time.Duration, seconds int) []int {
	counts := make([]int, seconds)
	for _, o := range offs {
		counts[int(o/time.Second)]++
	}
	return counts
}

func TestParseStage(t *testing.T) {
	cases := []struct {
		in     string
		dur    time.Duration
		target float64
		unit   Unit
		ok     bool
	}{
		{"30s:100", 30 * time.Second, 100, UnitNone, true},
		{"10s:250rps", 10 * time.Second, 250, UnitRPS, true},
		{"1m:50vu", time.Minute, 50, UnitVU, true},
		{" 2s : 5 VUs ", 2 * time.Second, 5, UnitVU, true},
		{"500ms:0.5rps", 500 * time.Millisecond, 0.5, UnitRPS, true},
		{"10s", 0, 0, 0, false},
		{"0s:10", 0, 0, 0, false},
		{"10s:-1", 0, 0, 0, false},
		{"10s:abc", 0, 0, 0, false},
		{"10s:2.5vu", 0, 0, 0, false},
	}
	for _, c := range cases {
		st, unit, err := ParseStage(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (st.Duration != c.dur || st.Target != c.target || unit != c.unit) {
			t.Errorf("%q: got %+v unit %v", c.in, st, unit)
		}
	}
}

func TestConstantRateIsEvenlySpaced(t *testing.T) {
	p, err := Constant(100, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	offs := collect(t, p)
	if len(offs) != 1000 {
		t.Fatalf("got %d arrivals, want 1000", len(offs))
	}
	for i, o := range offs {
		want := time.Duration(i) * 10 * time.Millisecond
		if d := o - want; d < -time.Microsecond || d > time.Microsecond {
			t.Fatalf("arrival %d at %v, want %v", i, o, want)
		}
	}
}

func TestFractionalRate(t *testing.T) {
	p, _ := Constant(0.5, 10*time.Second)
	offs := collect(t, p)
	if len(offs) != 5 || offs[1] != 2*time.Second {
		t.Fatalf("got %v", offs)
	}
}

func TestLinearRampMatchesIntegral(t *testing.T) {
	// 0 -> 100 rps over 10 s: rate(t) = 10t, expected arrivals in [k, k+1) = 5(2k+1).
	p, err := NewProfile(0, Stage{10 * time.Second, 100})
	if err != nil {
		t.Fatal(err)
	}
	offs := collect(t, p)
	if len(offs) != 500 {
		t.Fatalf("got %d arrivals, want 500", len(offs))
	}
	for k, got := range perSecond(offs, 10) {
		want := 5 * (2*k + 1)
		if got < want-1 || got > want+1 {
			t.Errorf("second %d: %d arrivals, want %d±1", k, got, want)
		}
	}
}

func TestRampUpHoldRampDown(t *testing.T) {
	p, err := NewProfile(0,
		Stage{2 * time.Second, 200}, // 200 arrivals
		Stage{3 * time.Second, 200}, // 600
		Stage{2 * time.Second, 50},  // 250
	)
	if err != nil {
		t.Fatal(err)
	}
	if p.Integral() != 1050 || p.Duration() != 7*time.Second || p.Max() != 200 {
		t.Fatalf("integral=%v duration=%v max=%v", p.Integral(), p.Duration(), p.Max())
	}
	offs := collect(t, p)
	if len(offs) != 1050 {
		t.Fatalf("got %d arrivals", len(offs))
	}
	counts := perSecond(offs, 7)
	want := []int{50, 150, 200, 200, 200, 162, 88} // integrals per second
	for i := range want {
		if d := counts[i] - want[i]; d < -1 || d > 1 {
			t.Errorf("second %d: %d arrivals, want %d±1 (all %v)", i, counts[i], want[i], counts)
		}
	}
}

func TestZeroRateStageIsSilent(t *testing.T) {
	p, _ := NewProfile(10, Stage{time.Second, 10}, Stage{time.Nanosecond, 0}, Stage{2 * time.Second, 0}, Stage{time.Second, 10})
	offs := collect(t, p)
	for _, o := range offs {
		if o > 1100*time.Millisecond && o < 3*time.Second {
			t.Fatalf("arrival at %v inside the zero-rate pause", o)
		}
	}
	if len(offs) != 15 {
		t.Fatalf("got %d arrivals, want 15", len(offs))
	}
}

func TestValueAtInterpolates(t *testing.T) {
	p, _ := NewProfile(10, Stage{10 * time.Second, 20}, Stage{5 * time.Second, 20})
	cases := map[time.Duration]float64{
		-time.Second:     10,
		0:                10,
		5 * time.Second:  15,
		10 * time.Second: 20,
		12 * time.Second: 20,
		99 * time.Second: 20,
	}
	for at, want := range cases {
		if got := p.ValueAt(at); math.Abs(got-want) > 1e-9 {
			t.Errorf("ValueAt(%v)=%v want %v", at, got, want)
		}
	}
	if got := p.Describe("rps"); got != "10s ramp 10->20 rps, 5s @ 20 rps" {
		t.Errorf("Describe: %q", got)
	}
}

func TestNewProfileErrors(t *testing.T) {
	if _, err := NewProfile(0); err == nil {
		t.Error("no stages should fail")
	}
	if _, err := NewProfile(-1, Stage{time.Second, 1}); err == nil {
		t.Error("negative start should fail")
	}
	if _, err := NewProfile(0, Stage{0, 1}); err == nil {
		t.Error("zero duration should fail")
	}
	if _, err := NewProfile(0, Stage{time.Second, math.NaN()}); err == nil {
		t.Error("NaN target should fail")
	}
}
