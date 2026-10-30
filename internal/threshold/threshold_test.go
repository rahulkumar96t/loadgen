package threshold

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in     string
		metric string
		op     string
		limit  float64
		unit   string
	}{
		{"p99>250ms", "p99", ">", 250, UnitMS},
		{" P99.9 >= 1.5s ", "p99.9", ">=", 1500, UnitMS},
		{"p999>1s", "p99.9", ">", 1000, UnitMS},
		{"p9999>1s", "p99.99", ">", 1000, UnitMS},
		{"p100>2s", "max", ">", 2000, UnitMS},
		{"max>500us", "max", ">", 0.5, UnitMS},
		{"service.p50>20ms", "service.p50", ">", 20, UnitMS},
		{"lag.p99>5ms", "lag.p99", ">", 5, UnitMS},
		{"error_rate>1%", "error_rate", ">", 0.01, UnitRatio},
		{"error_rate>0.05", "error_rate", ">", 0.05, UnitRatio},
		{"rps<95", "rps", "<", 95, UnitRPS},
		{"unsent>0", "unsent", ">", 0, UnitCount},
		{"mean<=10ms", "mean", "<=", 10, UnitMS},
	}
	for _, c := range cases {
		th, err := Parse(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if th.Metric != c.metric || th.Op != c.op || th.Limit != c.limit || th.Unit != c.unit {
			t.Errorf("%q: got %+v", c.in, th)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"p99",          // no operator
		"p99>250",      // latency without unit
		"p0>1ms",       // percentile out of range
		"p101>1ms",     // percentile out of range
		"latency>1ms",  // unknown metric
		"error_rate>5", // ratio > 1 without %
		"error_rate>abc%",
		"rps<-1",
		">1ms",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

type metrics map[string]float64

func (m metrics) Metric(name string) (float64, bool) {
	v, ok := m[name]
	return v, ok
}

func TestEvaluate(t *testing.T) {
	src := metrics{"p99": 300, "error_rate": 0.002, "rps": 99.5}
	var ts []Threshold
	for _, e := range []string{"p99>250ms", "error_rate>1%", "rps<95", "p99<=300ms"} {
		th, err := Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, th)
	}
	rs, err := Evaluate(ts, src)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, false, false, true}
	for i, r := range rs {
		if r.Breached != want[i] {
			t.Errorf("%s: breached=%v want %v (actual %v)", r.Expr, r.Breached, want[i], r.Actual)
		}
	}
	if !AnyBreached(rs) || AnyBreached(rs[1:3]) {
		t.Error("AnyBreached")
	}
	missing, _ := Parse("lag.p99>1ms")
	if _, err := Evaluate([]Threshold{missing}, src); err == nil || !strings.Contains(err.Error(), "lag.p99") {
		t.Errorf("missing metric should fail, got %v", err)
	}
}

func TestFormat(t *testing.T) {
	cases := map[string]string{
		Format(12.3456, UnitMS):   "12.346 ms",
		Format(0.0125, UnitRatio): "1.25%",
		Format(99.94, UnitRPS):    "99.9 rps",
		Format(3, UnitCount):      "3",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}
