// Package schedule turns load stages into a profile over time.
//
// A profile starts at a start value and passes through stages; each stage
// ramps linearly from the previous target to its own target over its duration
// (a stage whose target equals the previous one holds the value constant).
// For the open model the value is an arrival rate in requests per second and
// Arrivals yields the intended send offset of every request; for the closed
// model the value is a number of virtual users.
package schedule

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Stage ramps the profile to Target over Duration.
type Stage struct {
	Duration time.Duration
	Target   float64
}

// Unit is the optional unit suffix of a stage target.
type Unit int

// Stage target units.
const (
	UnitNone Unit = iota // a plain number
	UnitRPS              // "rps": requests per second (open model)
	UnitVU               // "vu"/"vus": virtual users (closed model)
)

// ParseStage parses "DURATION:TARGET[rps|vu]", e.g. "30s:200rps" or "1m:50vu".
func ParseStage(s string) (Stage, Unit, error) {
	d, t, ok := strings.Cut(s, ":")
	if !ok {
		return Stage{}, UnitNone, fmt.Errorf("stage %q: want DURATION:TARGET, e.g. 30s:100rps", s)
	}
	dur, err := time.ParseDuration(strings.TrimSpace(d))
	if err != nil || dur <= 0 {
		return Stage{}, UnitNone, fmt.Errorf("stage %q: invalid duration", s)
	}
	t = strings.ToLower(strings.TrimSpace(t))
	unit := UnitNone
	for _, sfx := range []struct {
		s string
		u Unit
	}{{"rps", UnitRPS}, {"vus", UnitVU}, {"vu", UnitVU}} {
		if rest, found := strings.CutSuffix(t, sfx.s); found {
			t, unit = strings.TrimSpace(rest), sfx.u
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return Stage{}, UnitNone, fmt.Errorf("stage %q: target must be a non-negative number", s)
	}
	if unit == UnitVU && v != math.Trunc(v) {
		return Stage{}, UnitNone, fmt.Errorf("stage %q: virtual users must be a whole number", s)
	}
	return Stage{Duration: dur, Target: v}, unit, nil
}

// String formats a stage the way ParseStage reads it (without a unit).
func (s Stage) String() string {
	return s.Duration.String() + ":" + strconv.FormatFloat(s.Target, 'f', -1, 64)
}

type segment struct {
	from     time.Duration // offset at which the segment starts
	dur      time.Duration
	v0, v1   float64
	cumStart float64 // integral of the profile before this segment
	area     float64 // integral of the profile over this segment
}

// Profile is a piecewise-linear function of time.
type Profile struct {
	start    float64
	segs     []segment
	total    time.Duration
	integral float64
}

// NewProfile builds a profile from a start value and one or more stages.
func NewProfile(start float64, stages ...Stage) (*Profile, error) {
	if len(stages) == 0 {
		return nil, errors.New("schedule: at least one stage is required")
	}
	if start < 0 || math.IsNaN(start) || math.IsInf(start, 0) {
		return nil, errors.New("schedule: start value must be a non-negative number")
	}
	p := &Profile{start: start}
	prev := start
	for i, st := range stages {
		if st.Duration <= 0 {
			return nil, fmt.Errorf("schedule: stage %d has non-positive duration", i+1)
		}
		if st.Target < 0 || math.IsNaN(st.Target) || math.IsInf(st.Target, 0) {
			return nil, fmt.Errorf("schedule: stage %d has invalid target", i+1)
		}
		area := (prev + st.Target) / 2 * st.Duration.Seconds()
		p.segs = append(p.segs, segment{from: p.total, dur: st.Duration, v0: prev, v1: st.Target, cumStart: p.integral, area: area})
		p.total += st.Duration
		p.integral += area
		prev = st.Target
	}
	return p, nil
}

// Constant is a profile that holds value for d.
func Constant(value float64, d time.Duration) (*Profile, error) {
	return NewProfile(value, Stage{Duration: d, Target: value})
}

// Duration is the total length of the profile.
func (p *Profile) Duration() time.Duration { return p.total }

// Integral is the area under the profile; for a rate profile this is the
// expected number of requests.
func (p *Profile) Integral() float64 { return p.integral }

// Max is the largest value the profile reaches.
func (p *Profile) Max() float64 {
	m := p.start
	for _, s := range p.segs {
		m = max(m, s.v1)
	}
	return m
}

// ValueAt returns the interpolated value at offset t.
func (p *Profile) ValueAt(t time.Duration) float64 {
	if t <= 0 {
		return p.start
	}
	for _, s := range p.segs {
		if t < s.from+s.dur {
			frac := float64(t-s.from) / float64(s.dur)
			return s.v0 + (s.v1-s.v0)*frac
		}
	}
	return p.segs[len(p.segs)-1].v1
}

// Describe renders the profile as a compact human-readable string.
func (p *Profile) Describe(unit string) string {
	var b strings.Builder
	for i, s := range p.segs {
		if i > 0 {
			b.WriteString(", ")
		}
		if s.v0 == s.v1 {
			fmt.Fprintf(&b, "%s @ %s %s", s.dur, num(s.v1), unit)
		} else {
			fmt.Fprintf(&b, "%s ramp %s->%s %s", s.dur, num(s.v0), num(s.v1), unit)
		}
	}
	return b.String()
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

const eps = 1e-6

// Arrivals returns an iterator over intended send offsets for a rate profile.
func (p *Profile) Arrivals() *Arrivals { return &Arrivals{p: p} }

// Count is the number of arrivals the profile produces.
func (p *Profile) Count() int64 {
	return int64(math.Ceil(p.integral - eps))
}

// Arrivals yields the intended send offset of request 0, 1, 2, ... The n-th
// request is due at the time t where the integral of the rate from 0 to t
// equals n, so the schedule depends only on the profile and never on how
// quickly earlier requests completed.
type Arrivals struct {
	p   *Profile
	n   int64
	seg int
}

// Next returns the next intended offset, or false when the profile is over.
func (a *Arrivals) Next() (time.Duration, bool) {
	n := float64(a.n)
	if n >= a.p.integral-eps {
		return 0, false
	}
	segs := a.p.segs
	for a.seg < len(segs)-1 && n >= segs[a.seg].cumStart+segs[a.seg].area {
		a.seg++
	}
	s := segs[a.seg]
	a.n++
	return s.from + s.solve(max(n-s.cumStart, 0)), true
}

// solve returns the offset within the segment at which the integral of the
// linearly changing rate reaches k:  v0*t + (v1-v0)/(2D)*t^2 = k.
// It uses the form 2k / (b + sqrt(b^2 + 4ak)) which is stable for a == 0,
// a < 0 (ramp down) and b == 0 (ramp up from zero).
func (s segment) solve(k float64) time.Duration {
	if k <= 0 {
		return 0
	}
	d := s.dur.Seconds()
	a := (s.v1 - s.v0) / (2 * d)
	b := s.v0
	disc := max(b*b+4*a*k, 0)
	den := b + math.Sqrt(disc)
	if den <= 0 {
		return s.dur
	}
	t := min(2*k/den, d)
	return time.Duration(t * float64(time.Second))
}
