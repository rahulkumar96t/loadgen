// Package report turns a run result into a summary and renders it as JSON,
// Markdown or plain text.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/hist"
	"github.com/rahulkumar96t/loadgen/internal/stats"
	"github.com/rahulkumar96t/loadgen/internal/threshold"
)

// RunConfig records how the run was configured.
type RunConfig struct {
	Model        string   `json:"model"`
	Targets      []string `json:"targets"`
	Profile      string   `json:"profile"`
	MaxInFlight  int      `json:"max_inflight,omitempty"`
	Think        string   `json:"think,omitempty"`
	Timeout      string   `json:"timeout"`
	ExpectStatus string   `json:"expect_status"`
	SigDigits    int      `json:"significant_digits"`
}

// Requests are the request counters of a run.
type Requests struct {
	Scheduled uint64 `json:"scheduled"`
	Sent      uint64 `json:"sent"`
	Unsent    uint64 `json:"unsent"`
	Completed uint64 `json:"completed"`
	OK        uint64 `json:"ok"`
	Failed    uint64 `json:"failed"`
}

// Dist summarises one latency distribution in milliseconds.
type Dist struct {
	Basis     string  `json:"basis"`
	Count     uint64  `json:"count"`
	Min       float64 `json:"min_ms"`
	Mean      float64 `json:"mean_ms"`
	P50       float64 `json:"p50_ms"`
	P90       float64 `json:"p90_ms"`
	P99       float64 `json:"p99_ms"`
	P999      float64 `json:"p99_9_ms"`
	Max       float64 `json:"max_ms"`
	Saturated uint64  `json:"saturated,omitempty"`
}

// Summary is the complete, serialisable report of a run.
type Summary struct {
	Tool        string             `json:"tool"`
	Version     string             `json:"version"`
	Config      RunConfig          `json:"config"`
	StartedAt   time.Time          `json:"started_at"`
	ElapsedSec  float64            `json:"elapsed_s"`
	Interrupted bool               `json:"interrupted,omitempty"`
	Requests    Requests           `json:"requests"`
	Throughput  float64            `json:"throughput_rps"`
	ErrorRate   float64            `json:"error_rate"`
	BytesIn     int64              `json:"bytes_in"`
	StatusCodes map[string]uint64  `json:"status_codes"`
	Errors      map[string]uint64  `json:"errors"`
	Latency     Dist               `json:"latency"`
	Service     *Dist              `json:"service_time,omitempty"`
	SendLag     *Dist              `json:"send_lag,omitempty"`
	Thresholds  []threshold.Result `json:"thresholds,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`

	latency, service, lag *hist.Histogram
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func toMS(ns int64) float64 { return round3(float64(ns) / 1e6) }

func dist(basis string, h *hist.Histogram) Dist {
	return Dist{
		Basis: basis, Count: h.Count(), Saturated: h.Saturated(),
		Min: toMS(h.Min()), Mean: round3(h.Mean() / 1e6),
		P50: toMS(h.ValueAtQuantile(0.5)), P90: toMS(h.ValueAtQuantile(0.9)),
		P99: toMS(h.ValueAtQuantile(0.99)), P999: toMS(h.ValueAtQuantile(0.999)),
		Max: toMS(h.Max()),
	}
}

// Build creates a summary from a run result.
func Build(res *stats.Result, cfg RunConfig, version string) *Summary {
	s := &Summary{
		Tool: "loadgen", Version: version, Config: cfg,
		StartedAt: res.Started.UTC().Truncate(time.Millisecond), ElapsedSec: round3(res.Elapsed.Seconds()),
		Interrupted: res.Interrupted,
		Requests: Requests{
			Scheduled: res.Scheduled, Sent: res.Sent, Unsent: res.Unsent,
			Completed: res.Completed, OK: res.Completed - res.Failed, Failed: res.Failed,
		},
		BytesIn:     res.BytesIn,
		StatusCodes: map[string]uint64{},
		Errors:      maps.Clone(res.Errors),
		latency:     res.Latency, service: res.Service, lag: res.Lag,
	}
	if s.Errors == nil {
		s.Errors = map[string]uint64{}
	}
	for code, n := range res.Statuses {
		s.StatusCodes[strconv.Itoa(code)] = n
	}
	if res.Elapsed > 0 {
		s.Throughput = round3(float64(res.Completed) / res.Elapsed.Seconds())
	}
	if res.Completed > 0 {
		s.ErrorRate = float64(res.Failed) / float64(res.Completed)
	}
	if res.Lag != nil {
		s.Latency = dist("intended_time", res.Latency)
		sv, lg := dist("service_time", res.Service), dist("send_lag", res.Lag)
		s.Service, s.SendLag = &sv, &lg
	} else {
		s.Latency = dist("service_time", res.Latency)
	}
	s.Warnings = warnings(s, res)
	return s
}

func warnings(s *Summary, res *stats.Result) []string {
	var w []string
	if res.Model == "closed" {
		w = append(w, "closed model: latency is service time only and hides queueing delay (coordinated omission); use the open model for latency objectives")
	}
	if res.Unsent > 0 {
		w = append(w, fmt.Sprintf("%d scheduled requests were never sent because max in-flight (%d) was reached; the target could not keep up with the offered rate", res.Unsent, s.Config.MaxInFlight))
	}
	if s.SendLag != nil && s.SendLag.P99 > 10 {
		w = append(w, fmt.Sprintf("send lag p99 is %.1f ms: requests left the generator late (in-flight limit reached or generator CPU-bound); latency is still measured from the intended time", s.SendLag.P99))
	}
	if s.Latency.Saturated > 0 {
		w = append(w, fmt.Sprintf("%d latencies exceeded %s and were clamped", s.Latency.Saturated, stats.HighestLatency))
	}
	if res.Interrupted {
		w = append(w, "run was interrupted before the profile finished")
	}
	if res.Completed == 0 {
		w = append(w, "no requests completed")
	}
	return w
}

// Metric implements threshold.Source.
func (s *Summary) Metric(name string) (float64, bool) {
	switch name {
	case "error_rate":
		return s.ErrorRate, true
	case "rps":
		return s.Throughput, true
	case "unsent":
		return float64(s.Requests.Unsent), true
	case "failed":
		return float64(s.Requests.Failed), true
	case "completed":
		return float64(s.Requests.Completed), true
	case "sent":
		return float64(s.Requests.Sent), true
	}
	h := s.latency
	if rest, ok := strings.CutPrefix(name, "service."); ok {
		h, name = s.service, rest
	} else if rest, ok := strings.CutPrefix(name, "lag."); ok {
		h, name = s.lag, rest
	}
	if h == nil {
		return 0, false
	}
	switch name {
	case "min":
		return toMS(h.Min()), true
	case "max":
		return toMS(h.Max()), true
	case "mean":
		return round3(h.Mean() / 1e6), true
	}
	q, err := strconv.ParseFloat(strings.TrimPrefix(name, "p"), 64)
	if err != nil || !strings.HasPrefix(name, "p") || q <= 0 || q > 100 {
		return 0, false
	}
	return toMS(h.ValueAtQuantile(q / 100)), true
}

// Breached reports whether any threshold failed.
func (s *Summary) Breached() bool { return threshold.AnyBreached(s.Thresholds) }

// WriteJSON writes the summary as indented JSON.
func (s *Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (s *Summary) dists() []Dist {
	out := []Dist{s.Latency}
	if s.Service != nil {
		out = append(out, *s.Service)
	}
	if s.SendLag != nil {
		out = append(out, *s.SendLag)
	}
	return out
}

var basisLabel = map[string]string{
	"intended_time": "latency (from intended send time)",
	"service_time":  "service time",
	"send_lag":      "send lag",
}

func f3(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func sortedKeys(m map[string]uint64) []string { return slices.Sorted(maps.Keys(m)) }

// WriteMarkdown writes a GitHub-flavoured Markdown report.
func (s *Summary) WriteMarkdown(w io.Writer) error {
	var b strings.Builder
	b.WriteString("# Load test report\n\n")
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Tool | %s %s |\n", s.Tool, s.Version)
	fmt.Fprintf(&b, "| Model | %s |\n", s.Config.Model)
	for _, t := range s.Config.Targets {
		fmt.Fprintf(&b, "| Target | `%s` |\n", t)
	}
	fmt.Fprintf(&b, "| Profile | %s |\n", s.Config.Profile)
	fmt.Fprintf(&b, "| Started | %s |\n", s.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "| Elapsed | %.3f s |\n", s.ElapsedSec)
	if s.Config.MaxInFlight > 0 {
		fmt.Fprintf(&b, "| Max in-flight | %d |\n", s.Config.MaxInFlight)
	}
	if s.Config.Think != "" {
		fmt.Fprintf(&b, "| Think time | %s |\n", s.Config.Think)
	}
	fmt.Fprintf(&b, "| Timeout | %s |\n| Expected status | %s |\n", s.Config.Timeout, s.Config.ExpectStatus)

	r := s.Requests
	b.WriteString("\n## Requests\n\n")
	b.WriteString("| Scheduled | Sent | Unsent | Completed | OK | Failed | Error rate | Throughput |\n")
	b.WriteString("|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d | %s | %s |\n", r.Scheduled, r.Sent, r.Unsent, r.Completed, r.OK, r.Failed,
		threshold.Format(s.ErrorRate, threshold.UnitRatio), threshold.Format(s.Throughput, threshold.UnitRPS))

	b.WriteString("\n## Latency (ms)\n\n")
	b.WriteString("| Measure | Count | Min | Mean | p50 | p90 | p99 | p99.9 | Max |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, d := range s.dists() {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %s | %s |\n", basisLabel[d.Basis], d.Count,
			f3(d.Min), f3(d.Mean), f3(d.P50), f3(d.P90), f3(d.P99), f3(d.P999), f3(d.Max))
	}

	b.WriteString("\n## Status codes\n\n")
	if len(s.StatusCodes) == 0 {
		b.WriteString("None.\n")
	} else {
		b.WriteString("| Code | Count |\n|---|---:|\n")
		for _, k := range sortedKeys(s.StatusCodes) {
			fmt.Fprintf(&b, "| %s | %d |\n", k, s.StatusCodes[k])
		}
	}
	b.WriteString("\n## Transport errors\n\n")
	if len(s.Errors) == 0 {
		b.WriteString("None.\n")
	} else {
		b.WriteString("| Class | Count |\n|---|---:|\n")
		for _, k := range sortedKeys(s.Errors) {
			fmt.Fprintf(&b, "| %s | %d |\n", k, s.Errors[k])
		}
	}
	if len(s.Thresholds) > 0 {
		b.WriteString("\n## Thresholds\n\n| Fail if | Actual | Result |\n|---|---:|---|\n")
		for _, t := range s.Thresholds {
			res := "PASS"
			if t.Breached {
				res = "FAIL"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", t.Expr, threshold.Format(t.Actual, t.Unit), res)
		}
	}
	if len(s.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range s.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteText writes a compact terminal summary.
func (s *Summary) WriteText(w io.Writer) error {
	var b strings.Builder
	r := s.Requests
	fmt.Fprintf(&b, "%s %s  model=%s  %s\n", s.Tool, s.Version, s.Config.Model, strings.Join(s.Config.Targets, ", "))
	fmt.Fprintf(&b, "profile: %s\n", s.Config.Profile)
	fmt.Fprintf(&b, "elapsed %.2fs  scheduled %d  sent %d  unsent %d  completed %d  failed %d (%s)  throughput %s\n\n",
		s.ElapsedSec, r.Scheduled, r.Sent, r.Unsent, r.Completed, r.Failed,
		threshold.Format(s.ErrorRate, threshold.UnitRatio), threshold.Format(s.Throughput, threshold.UnitRPS))
	fmt.Fprintf(&b, "%-10s %10s %10s %10s %10s %10s %10s %10s\n", "ms", "min", "mean", "p50", "p90", "p99", "p99.9", "max")
	short := map[string]string{"intended_time": "latency", "service_time": "service", "send_lag": "send lag"}
	for _, d := range s.dists() {
		fmt.Fprintf(&b, "%-10s %10s %10s %10s %10s %10s %10s %10s\n", short[d.Basis], f3(d.Min), f3(d.Mean), f3(d.P50), f3(d.P90), f3(d.P99), f3(d.P999), f3(d.Max))
	}
	b.WriteString("\nstatus:")
	if len(s.StatusCodes) == 0 {
		b.WriteString(" none")
	}
	for _, k := range sortedKeys(s.StatusCodes) {
		fmt.Fprintf(&b, " %s=%d", k, s.StatusCodes[k])
	}
	b.WriteString("\nerrors:")
	if len(s.Errors) == 0 {
		b.WriteString(" none")
	}
	for _, k := range sortedKeys(s.Errors) {
		fmt.Fprintf(&b, " %s=%d", k, s.Errors[k])
	}
	b.WriteString("\n")
	for _, t := range s.Thresholds {
		res := "PASS"
		if t.Breached {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "threshold %s  %-20s actual %s\n", res, t.Expr, threshold.Format(t.Actual, t.Unit))
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
