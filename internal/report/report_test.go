package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/hist"
	"github.com/rahulkumar96t/loadgen/internal/stats"
	"github.com/rahulkumar96t/loadgen/internal/threshold"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fixture is a deterministic open-model result: 1000 requests, latencies
// 1..1000 ms from the intended time, service time = latency/2, lag = latency/2.
func fixture() *stats.Result {
	mk := func() *hist.Histogram { return hist.MustNew(3, int64(stats.HighestLatency)) }
	lat, svc, lag := mk(), mk(), mk()
	for i := int64(1); i <= 1000; i++ {
		lat.Record(i * int64(time.Millisecond))
		svc.Record(i * int64(time.Millisecond) / 2)
		lag.Record(i * int64(time.Millisecond) / 2)
	}
	return &stats.Result{
		Model: "open", Started: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Elapsed: 10 * time.Second,
		Scheduled: 1010, Sent: 1000, Unsent: 10, Completed: 1000, Failed: 12, BytesIn: 3000,
		Statuses: map[int]uint64{200: 990, 503: 2}, Errors: map[string]uint64{"timeout": 8, "connection_reset": 2},
		Latency: lat, Service: svc, Lag: lag,
	}
}

// near allows for the histogram's bounded over-estimate (0.1% at 3 digits).
func near(got, exact float64) bool { return got >= exact && got <= exact*1.001 }

func cfg() RunConfig {
	return RunConfig{Model: "open", Targets: []string{"GET http://127.0.0.1:8080/"}, Profile: "10s @ 101 rps",
		MaxInFlight: 64, Timeout: "10s", ExpectStatus: "200-399", SigDigits: 3}
}

func summary(t *testing.T) *Summary {
	t.Helper()
	s := Build(fixture(), cfg(), "test")
	var ts []threshold.Threshold
	for _, e := range []string{"p99>250ms", "error_rate>5%", "service.p50>1s"} {
		th, err := threshold.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, th)
	}
	rs, err := threshold.Evaluate(ts, s)
	if err != nil {
		t.Fatal(err)
	}
	s.Thresholds = rs
	return s
}

func TestBuildComputesRates(t *testing.T) {
	s := summary(t)
	if s.Throughput != 100 || s.ErrorRate != 0.012 || s.Requests.OK != 988 {
		t.Fatalf("throughput=%v error_rate=%v ok=%d", s.Throughput, s.ErrorRate, s.Requests.OK)
	}
	if s.Latency.Basis != "intended_time" || s.Service == nil || s.SendLag == nil {
		t.Fatal("open model must report intended-time latency, service time and send lag")
	}
	if !near(s.Latency.P50, 500) || !near(s.Latency.P99, 990) || s.Latency.Max != 1000 || s.Latency.Min != 1 {
		t.Fatalf("latency %+v", s.Latency)
	}
	if !s.Breached() || !near(s.Thresholds[0].Actual, 990) || s.Thresholds[1].Breached || s.Thresholds[2].Breached {
		t.Fatalf("thresholds %+v", s.Thresholds)
	}
	// 10 unsent + lag p99 495 ms -> two warnings.
	if len(s.Warnings) != 2 {
		t.Fatalf("warnings %q", s.Warnings)
	}
}

func TestMetricNames(t *testing.T) {
	s := summary(t)
	cases := map[string]float64{
		"p50": 500, "p99.9": 999, "max": 1000, "min": 1, "mean": 500.5,
		"service.p50": 250, "lag.max": 500, "error_rate": 0.012, "rps": 100, "unsent": 10,
	}
	for name, want := range cases {
		got, ok := s.Metric(name)
		if !ok || !near(got, want) {
			t.Errorf("Metric(%q) = %v, %v; want ~%v", name, got, ok, want)
		}
	}
	for _, bad := range []string{"bogus", "p0", "pfoo", "lag.p101"} {
		if _, ok := s.Metric(bad); ok {
			t.Errorf("Metric(%q) should be unknown", bad)
		}
	}
	closed := fixture()
	closed.Model, closed.Lag, closed.Service = "closed", nil, closed.Latency
	cs := Build(closed, cfg(), "test")
	if _, ok := cs.Metric("lag.p99"); ok || cs.Service != nil || cs.Latency.Basis != "service_time" {
		t.Fatal("closed model has no send lag and reports service time as latency")
	}
	if !strings.Contains(strings.Join(cs.Warnings, " "), "coordinated omission") {
		t.Fatalf("closed model should warn about coordinated omission: %q", cs.Warnings)
	}
}

func TestJSONReport(t *testing.T) {
	var buf bytes.Buffer
	if err := summary(t).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tool", "version", "config", "started_at", "elapsed_s", "requests", "throughput_rps",
		"error_rate", "status_codes", "errors", "latency", "service_time", "send_lag", "thresholds", "warnings"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	lat := got["latency"].(map[string]any)
	if lat["basis"] != "intended_time" || !near(lat["p99_ms"].(float64), 990) || !near(lat["p99_9_ms"].(float64), 999) {
		t.Errorf("latency %v", lat)
	}
	if got["status_codes"].(map[string]any)["503"] != 2.0 {
		t.Errorf("status codes %v", got["status_codes"])
	}
	th := got["thresholds"].([]any)[0].(map[string]any)
	if th["expr"] != "p99>250ms" || th["breached"] != true || th["unit"] != "ms" {
		t.Errorf("threshold %v", th)
	}
}

func TestMarkdownGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := summary(t).WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "report.golden.md")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/report -update)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("markdown differs from %s; got:\n%s", golden, buf.String())
	}
}

func TestTextReport(t *testing.T) {
	var buf bytes.Buffer
	if err := summary(t).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"model=open", "unsent 10", "failed 12 (1.20%)", "throughput 100.0 rps",
		"latency", "service", "send lag", "status: 200=990 503=2", "errors: connection_reset=2 timeout=8",
		"threshold FAIL  p99>250ms", "threshold PASS  error_rate>5%", "warning: 10 scheduled requests were never sent",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q:\n%s", want, out)
		}
	}
}
