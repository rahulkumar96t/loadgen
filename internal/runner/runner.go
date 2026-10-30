// Package runner drives load according to the open or the closed model.
//
// Open model: requests arrive on a schedule computed from the rate profile,
// independent of how fast the target answers. Each request carries its
// intended send time and latency is measured from it, so a stalled server (or
// a generator that falls behind) shows up in the latency distribution instead
// of silently lowering the request rate. This avoids coordinated omission.
//
// Closed model: N virtual users each send a request, wait for the response,
// optionally think, and repeat. Throughput adapts to the server, which is what
// you want for capacity exploration but not for latency SLOs.
package runner

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/request"
	"github.com/rahulkumar96t/loadgen/internal/schedule"
	"github.com/rahulkumar96t/loadgen/internal/stats"
)

// Model selects how load is generated.
type Model string

// Load models.
const (
	Open   Model = "open"
	Closed Model = "closed"
)

// Config describes a run.
type Config struct {
	Model    Model
	Profile  *schedule.Profile // requests/second (open) or virtual users (closed)
	Requests *request.Set
	Expect   stats.StatusSet // statuses counted as success (default 200-399)
	Digits   int             // histogram precision, default 3

	// MaxInFlight caps concurrent requests in the open model (0 = unlimited).
	// When the cap is reached the dispatcher waits for a free slot; requests
	// still waiting when the profile ends are counted as unsent.
	MaxInFlight int
	Think       time.Duration // closed model pause between requests
	Timeout     time.Duration // per request, default 10s

	Client *http.Client // optional; built from ClientOptions when nil
	Clock  Clock        // optional; real time when nil
}

// ClientOptions configures the default HTTP client.
type ClientOptions struct {
	Timeout          time.Duration
	MaxConns         int // per host; 0 = unlimited
	Insecure         bool
	DisableKeepAlive bool
}

// NewHTTPClient builds a client tuned for load generation: connection reuse
// sized to the concurrency, HTTP/1.1, no redirect following.
func NewHTTPClient(o ClientOptions) *http.Client {
	idle := o.MaxConns
	if idle <= 0 {
		idle = 1024
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: o.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   idle,
		MaxConnsPerHost:       o.MaxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   o.Timeout,
		DisableKeepAlives:     o.DisableKeepAlive,
		ExpectContinueTimeout: time.Second,
	}
	if o.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in flag for test targets
	}
	return &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Progress is a live snapshot of a running test.
type Progress struct {
	Elapsed   time.Duration
	Sent      int64
	Completed uint64
	Failed    uint64
	InFlight  int64
	Unsent    int64
	Target    float64 // current target rate (open) or VU count (closed)
}

// Runner executes one run. Create it with New and call Run once.
type Runner struct {
	cfg   Config
	rec   *stats.Recorder
	clock Clock

	startNanos atomic.Int64
	seq        atomic.Uint64
	scheduled  atomic.Int64
	sent       atomic.Int64
	unsent     atomic.Int64
	inflight   atomic.Int64
	peak       atomic.Int64
}

// New validates the configuration and prepares a runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Model != Open && cfg.Model != Closed {
		return nil, errors.New("runner: model must be open or closed")
	}
	if cfg.Profile == nil || cfg.Requests == nil {
		return nil, errors.New("runner: profile and requests are required")
	}
	if cfg.MaxInFlight < 0 || cfg.Think < 0 || cfg.Timeout < 0 {
		return nil, errors.New("runner: negative max in-flight, think time or timeout")
	}
	if cfg.Digits == 0 {
		cfg.Digits = 3
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Expect.IsZero() {
		cfg.Expect = stats.DefaultStatusSet
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Client == nil {
		conns := cfg.MaxInFlight
		if cfg.Model == Closed {
			conns = int(math.Ceil(cfg.Profile.Max()))
		}
		cfg.Client = NewHTTPClient(ClientOptions{Timeout: cfg.Timeout, MaxConns: conns})
	}
	rec, err := stats.NewRecorder(cfg.Model == Open, cfg.Digits, cfg.Expect)
	if err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, rec: rec, clock: cfg.Clock}, nil
}

// Run generates load until the profile ends (or ctx is cancelled), waits for
// in-flight requests and returns the aggregated result.
func (r *Runner) Run(ctx context.Context) *stats.Result {
	start := r.clock.Now()
	r.startNanos.Store(start.UnixNano())
	if r.cfg.Model == Open {
		r.runOpen(ctx, start)
	} else {
		r.runClosed(ctx, start)
	}
	res := r.rec.Result()
	res.Started = start
	res.Elapsed = r.clock.Now().Sub(start)
	res.Interrupted = ctx.Err() != nil
	res.Scheduled = uint64(r.scheduled.Load())
	res.Sent = uint64(r.sent.Load())
	res.Unsent = uint64(r.unsent.Load())
	res.PeakConc = r.peak.Load()
	return res
}

// Progress returns live counters; safe to call concurrently with Run.
func (r *Runner) Progress() Progress {
	completed, failed := r.rec.Counts()
	p := Progress{
		Sent: r.sent.Load(), Completed: completed, Failed: failed,
		InFlight: r.inflight.Load(), Unsent: r.unsent.Load(),
	}
	if s := r.startNanos.Load(); s != 0 {
		p.Elapsed = r.clock.Now().Sub(time.Unix(0, s))
		p.Target = r.cfg.Profile.ValueAt(p.Elapsed)
	}
	return p
}

func (r *Runner) notePeak(v int64) {
	for {
		p := r.peak.Load()
		if v <= p || r.peak.CompareAndSwap(p, v) {
			return
		}
	}
}

func (r *Runner) runOpen(ctx context.Context, start time.Time) {
	arrivals := r.cfg.Profile.Arrivals()
	deadline := start.Add(r.cfg.Profile.Duration())
	var slots chan struct{}
	if r.cfg.MaxInFlight > 0 {
		slots = make(chan struct{}, r.cfg.MaxInFlight)
	}
	slotCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		off, ok := arrivals.Next()
		if !ok {
			return
		}
		intended := start.Add(off)
		if err := r.clock.SleepUntil(ctx, intended); err != nil {
			return // interrupted: arrivals not yet due are not counted
		}
		r.scheduled.Add(1)
		if slots != nil && !acquire(ctx, slotCtx, slots) {
			if ctx.Err() != nil {
				return
			}
			// The profile ended while we were still waiting for a slot:
			// this and every remaining arrival were never sent.
			n := int64(1)
			for _, ok := arrivals.Next(); ok; _, ok = arrivals.Next() {
				n++
			}
			r.scheduled.Add(n - 1)
			r.unsent.Add(n)
			return
		}
		seq := r.seq.Add(1) - 1
		r.sent.Add(1)
		r.notePeak(r.inflight.Add(1))
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.do(ctx, seq, intended)
			r.inflight.Add(-1)
			if slots != nil {
				<-slots
			}
		}()
	}
}

// acquire takes an in-flight slot, waiting until one frees up or until
// slotCtx (the end of the profile) is done. A free slot always wins.
func acquire(ctx, slotCtx context.Context, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-slotCtx.Done():
		return false
	case <-ctx.Done():
		return false
	}
}

// vuTick is how often the closed model re-evaluates the target VU count.
const vuTick = 50 * time.Millisecond

func (r *Runner) runClosed(ctx context.Context, start time.Time) {
	total := r.cfg.Profile.Duration()
	deadline := start.Add(total)
	var wg sync.WaitGroup
	var stops []chan struct{}
	defer func() {
		for _, s := range stops {
			close(s)
		}
		wg.Wait()
	}()
	for {
		now := r.clock.Now()
		elapsed := now.Sub(start)
		if elapsed >= total || ctx.Err() != nil {
			return
		}
		want := int(math.Round(r.cfg.Profile.ValueAt(elapsed)))
		for len(stops) < want {
			stop := make(chan struct{})
			stops = append(stops, stop)
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.virtualUser(ctx, stop, deadline)
			}()
		}
		for len(stops) > want {
			close(stops[len(stops)-1])
			stops = stops[:len(stops)-1]
		}
		r.notePeak(int64(len(stops)))
		next := now.Add(vuTick)
		if next.After(deadline) {
			next = deadline
		}
		if err := r.clock.SleepUntil(ctx, next); err != nil {
			return
		}
	}
}

func (r *Runner) virtualUser(ctx context.Context, stop <-chan struct{}, deadline time.Time) {
	var think *time.Timer
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		if !r.clock.Now().Before(deadline) {
			return
		}
		seq := r.seq.Add(1) - 1
		r.scheduled.Add(1)
		r.sent.Add(1)
		r.inflight.Add(1)
		r.do(ctx, seq, time.Time{})
		r.inflight.Add(-1)
		if r.cfg.Think > 0 {
			if think == nil {
				think = time.NewTimer(r.cfg.Think)
				defer think.Stop()
			} else {
				think.Reset(r.cfg.Think)
			}
			select {
			case <-think.C:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

func (r *Runner) do(ctx context.Context, seq uint64, intended time.Time) {
	req, err := r.cfg.Requests.Build(ctx, seq, time.Now())
	start := r.clock.Now()
	if intended.IsZero() {
		intended = start
	}
	o := stats.Outcome{Intended: intended, Start: start}
	if err == nil {
		var resp *http.Response
		resp, err = r.cfg.Client.Do(req)
		if err == nil {
			o.Status = resp.StatusCode
			o.Bytes, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	o.Err = err
	o.End = r.clock.Now()
	r.rec.Record(o)
}
