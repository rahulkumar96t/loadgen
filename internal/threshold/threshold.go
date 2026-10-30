// Package threshold parses and evaluates pass/fail criteria such as
// "p99>250ms" or "error_rate>1%". A threshold describes the failure condition:
// it is breached when the expression is true for the measured value.
package threshold

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Unit of a metric.
const (
	UnitMS    = "ms"
	UnitRatio = "ratio"
	UnitRPS   = "rps"
	UnitCount = "count"
)

// Threshold is a parsed expression.
type Threshold struct {
	Expr   string
	Metric string // normalised metric name, e.g. "p99.9" or "service.p99"
	Op     string // one of > >= < <=
	Limit  float64
	Unit   string
}

// Source provides metric values. Latency metrics are in milliseconds and
// ratios are fractions in [0, 1].
type Source interface {
	Metric(name string) (float64, bool)
}

// Result is the outcome of evaluating one threshold.
type Result struct {
	Expr     string  `json:"expr"`
	Metric   string  `json:"metric"`
	Actual   float64 `json:"actual"`
	Limit    float64 `json:"limit"`
	Unit     string  `json:"unit"`
	Breached bool    `json:"breached"`
}

// Parse parses "METRIC OP VALUE". Latency metrics are p<N> (p50, p99, p99.9,
// p999), min, mean and max, optionally prefixed with "service." or "lag.";
// their value needs a duration unit (250ms, 1.5s). error_rate takes a percent
// or a fraction; rps, unsent, failed and completed take plain numbers.
func Parse(expr string) (Threshold, error) {
	i := strings.IndexAny(expr, "<>")
	if i < 0 {
		return Threshold{}, fmt.Errorf("threshold %q: want METRIC>VALUE, e.g. p99>250ms or error_rate>1%%", expr)
	}
	op := expr[i : i+1]
	rest := expr[i+1:]
	if strings.HasPrefix(rest, "=") {
		op += "="
		rest = rest[1:]
	}
	metric, unit, err := normaliseMetric(strings.ToLower(strings.TrimSpace(expr[:i])))
	if err != nil {
		return Threshold{}, fmt.Errorf("threshold %q: %w", expr, err)
	}
	limit, err := parseValue(strings.TrimSpace(rest), unit)
	if err != nil {
		return Threshold{}, fmt.Errorf("threshold %q: %w", expr, err)
	}
	return Threshold{Expr: strings.TrimSpace(expr), Metric: metric, Op: op, Limit: limit, Unit: unit}, nil
}

func normaliseMetric(m string) (string, string, error) {
	switch m {
	case "error_rate", "errors":
		return "error_rate", UnitRatio, nil
	case "rps", "throughput":
		return "rps", UnitRPS, nil
	case "unsent", "failed", "completed", "sent":
		return m, UnitCount, nil
	}
	prefix := ""
	for _, p := range []string{"service.", "lag."} {
		if rest, ok := strings.CutPrefix(m, p); ok {
			prefix, m = p, rest
		}
	}
	switch m {
	case "min", "mean", "max":
		return prefix + m, UnitMS, nil
	}
	digits, ok := strings.CutPrefix(m, "p")
	if !ok || digits == "" {
		return "", "", fmt.Errorf("unknown metric %q", m)
	}
	// p999 and p9999 are shorthand for p99.9 and p99.99.
	if !strings.Contains(digits, ".") && len(digits) > 2 && strings.HasPrefix(digits, "99") {
		digits = "99." + digits[2:]
	}
	q, err := strconv.ParseFloat(digits, 64)
	if err != nil || q <= 0 || q > 100 {
		return "", "", fmt.Errorf("unknown metric %q (percentiles are p1..p100)", m)
	}
	if q == 100 {
		return prefix + "max", UnitMS, nil
	}
	return prefix + "p" + strconv.FormatFloat(q, 'f', -1, 64), UnitMS, nil
}

func parseValue(v, unit string) (float64, error) {
	switch unit {
	case UnitMS:
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("latency limit %q needs a unit, e.g. 250ms or 1.5s", v)
		}
		return float64(d) / float64(time.Millisecond), nil
	case UnitRatio:
		if p, ok := strings.CutSuffix(v, "%"); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil || f < 0 || f > 100 {
				return 0, fmt.Errorf("invalid percentage %q", v)
			}
			return f / 100, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return 0, fmt.Errorf("ratio %q must be a fraction in [0,1] or a percentage like 1%%", v)
		}
		return f, nil
	default:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid number %q", v)
		}
		return f, nil
	}
}

// Breached reports whether actual satisfies the failure expression.
func (t Threshold) Breached(actual float64) bool {
	switch t.Op {
	case ">":
		return actual > t.Limit
	case ">=":
		return actual >= t.Limit
	case "<":
		return actual < t.Limit
	default:
		return actual <= t.Limit
	}
}

// Evaluate checks every threshold against src.
func Evaluate(ts []Threshold, src Source) ([]Result, error) {
	out := make([]Result, 0, len(ts))
	for _, t := range ts {
		v, ok := src.Metric(t.Metric)
		if !ok {
			return nil, fmt.Errorf("threshold %q: metric %q is not available for this run", t.Expr, t.Metric)
		}
		out = append(out, Result{Expr: t.Expr, Metric: t.Metric, Actual: v, Limit: t.Limit, Unit: t.Unit, Breached: t.Breached(v)})
	}
	return out, nil
}

// AnyBreached reports whether at least one result failed.
func AnyBreached(rs []Result) bool {
	for _, r := range rs {
		if r.Breached {
			return true
		}
	}
	return false
}

// Format renders a value in its unit for humans.
func Format(v float64, unit string) string {
	switch unit {
	case UnitMS:
		return strconv.FormatFloat(v, 'f', 3, 64) + " ms"
	case UnitRatio:
		return strconv.FormatFloat(v*100, 'f', 2, 64) + "%"
	case UnitRPS:
		return strconv.FormatFloat(v, 'f', 1, 64) + " rps"
	default:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
}
