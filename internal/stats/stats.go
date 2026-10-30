// Package stats aggregates request outcomes into histograms and counters.
package stats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/hist"
)

// HighestLatency is the largest latency the histograms can track; anything
// slower is clamped and reported as saturated.
const HighestLatency = time.Hour

// Outcome is the result of one request.
type Outcome struct {
	Intended time.Time // when the schedule wanted the request sent (open model)
	Start    time.Time // when it was actually handed to the HTTP client
	End      time.Time // when the response body was fully read or the request failed
	Status   int
	Err      error
	Bytes    int64
}

// Recorder aggregates outcomes. It is safe for concurrent use.
//
// In the open model three distributions are kept:
//   - latency: End - Intended (what a user arriving on schedule experiences,
//     including time spent waiting because the system or generator was busy);
//   - service: End - Start (time inside the HTTP exchange only);
//   - lag:     Start - Intended (how late the generator sent the request).
//
// In the closed model there is no schedule, so latency is the service time.
type Recorder struct {
	mu        sync.Mutex
	open      bool
	expect    StatusSet
	latency   *hist.Histogram
	service   *hist.Histogram
	lag       *hist.Histogram
	statuses  map[int]uint64
	errs      map[string]uint64
	completed uint64
	failed    uint64
	bytes     int64
}

// NewRecorder creates a recorder. digits is the histogram precision (1..5).
func NewRecorder(open bool, digits int, expect StatusSet) (*Recorder, error) {
	mk := func() (*hist.Histogram, error) { return hist.New(digits, int64(HighestLatency)) }
	r := &Recorder{open: open, expect: expect, statuses: map[int]uint64{}, errs: map[string]uint64{}}
	var err error
	if r.service, err = mk(); err != nil {
		return nil, err
	}
	r.latency = r.service
	if open {
		r.latency, _ = mk()
		r.lag, _ = mk()
	}
	return r, nil
}

// Record adds one outcome.
func (r *Recorder) Record(o Outcome) {
	class := ClassifyError(o.Err)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	r.bytes += o.Bytes
	if o.Err != nil {
		r.errs[class]++
		r.failed++
	} else {
		r.statuses[o.Status]++
		if !r.expect.Contains(o.Status) {
			r.failed++
		}
	}
	r.service.Record(int64(o.End.Sub(o.Start)))
	if r.open {
		r.latency.Record(int64(o.End.Sub(o.Intended)))
		r.lag.Record(int64(o.Start.Sub(o.Intended)))
	}
}

// Counts returns completed and failed counts so far.
func (r *Recorder) Counts() (completed, failed uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completed, r.failed
}

// Result is the final, immutable outcome of a run.
type Result struct {
	Model       string
	Started     time.Time
	Elapsed     time.Duration // start of the run until the last response
	Interrupted bool

	Scheduled uint64 // requests the profile asked for (reached before the end)
	Sent      uint64 // requests handed to the HTTP client
	Unsent    uint64 // scheduled but never sent because max in-flight was reached
	Completed uint64
	Failed    uint64 // transport errors plus unexpected status codes
	BytesIn   int64
	PeakConc  int64 // peak in-flight requests (open) or virtual users (closed)

	Statuses map[int]uint64
	Errors   map[string]uint64

	Latency *hist.Histogram // intended-time latency (open) or service time (closed)
	Service *hist.Histogram // service time; same as Latency in the closed model
	Lag     *hist.Histogram // send lag; nil in the closed model
}

// Result snapshots the recorder. The histograms are shared, so callers must
// not record more outcomes afterwards.
func (r *Recorder) Result() *Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	model := "closed"
	if r.open {
		model = "open"
	}
	return &Result{
		Model:     model,
		Completed: r.completed,
		Failed:    r.failed,
		BytesIn:   r.bytes,
		Statuses:  maps.Clone(r.statuses),
		Errors:    maps.Clone(r.errs),
		Latency:   r.latency,
		Service:   r.service,
		Lag:       r.lag,
	}
}

// ClassifyError maps a transport error to a short, stable class name.
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	var (
		dnsErr   *net.DNSError
		netErr   net.Error
		certErr  *tls.CertificateVerificationError
		unknownA x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		recErr   tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_reset"
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return "too_many_open_files"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "address_unavailable"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &certErr), errors.As(err, &unknownA), errors.As(err, &hostErr), errors.As(err, &recErr):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	return "other"
}

// StatusSet is a set of HTTP status codes, e.g. "200-299,304".
type StatusSet struct{ ranges [][2]int }

// DefaultStatusSet treats 2xx and 3xx responses as successful.
var DefaultStatusSet = StatusSet{ranges: [][2]int{{200, 399}}}

// ParseStatusSet parses a comma-separated list of codes and inclusive ranges.
func ParseStatusSet(s string) (StatusSet, error) {
	var set StatusSet
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(item, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := a, error(nil)
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 100 || b > 599 || a > b {
			return StatusSet{}, fmt.Errorf("invalid status code range %q", item)
		}
		set.ranges = append(set.ranges, [2]int{a, b})
	}
	if len(set.ranges) == 0 {
		return StatusSet{}, fmt.Errorf("empty status set %q", s)
	}
	return set, nil
}

// IsZero reports whether the set is empty (the zero value).
func (s StatusSet) IsZero() bool { return len(s.ranges) == 0 }

// Contains reports whether code is in the set.
func (s StatusSet) Contains(code int) bool {
	for _, r := range s.ranges {
		if code >= r[0] && code <= r[1] {
			return true
		}
	}
	return false
}

func (s StatusSet) String() string {
	parts := make([]string, len(s.ranges))
	for i, r := range s.ranges {
		if r[0] == r[1] {
			parts[i] = strconv.Itoa(r[0])
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r[0], r[1])
		}
	}
	return strings.Join(parts, ",")
}
