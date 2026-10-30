// Package demo is a small HTTP target with configurable latency, errors and
// periodic stalls, used for demos and end-to-end tests.
package demo

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls the behaviour of the demo server.
type Config struct {
	Latency   time.Duration // base service time
	Jitter    time.Duration // extra uniformly random delay in [0, Jitter)
	ErrorRate float64       // fraction of requests answered with 500
	// StallEvery/StallFor simulate a periodic freeze (a GC pause, a lock, a
	// failover): during the last StallFor of every StallEvery period no request
	// is answered; requests arriving in the window wait until it ends.
	StallEvery time.Duration
	StallFor   time.Duration
	BodySize   int    // response body size in bytes (default 3: "ok\n")
	Seed       uint64 // random seed for jitter and errors (0 = fixed default)
}

// Validate reports configuration errors.
func (c Config) Validate() error {
	switch {
	case c.Latency < 0 || c.Jitter < 0:
		return errors.New("latency and jitter must be >= 0")
	case c.ErrorRate < 0 || c.ErrorRate > 1:
		return errors.New("error rate must be in [0, 1]")
	case c.StallFor < 0 || c.StallEvery < 0:
		return errors.New("stall durations must be >= 0")
	case c.StallFor > 0 && c.StallFor >= c.StallEvery:
		return errors.New("stall-for must be shorter than stall-every")
	case c.BodySize < 0:
		return errors.New("body size must be >= 0")
	}
	return nil
}

// Server is an http.Handler. Query parameters override the configuration
// per request: ?latency=50ms&status=404.
type Server struct {
	cfg   Config
	start time.Time
	body  []byte

	mu  sync.Mutex
	rng *rand.Rand

	requests atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
}

// New creates a server; the stall clock starts now.
func New(cfg Config) *Server {
	size := cfg.BodySize
	if size == 0 {
		size = 3
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = 'x'
	}
	if size >= 3 {
		copy(body, "ok")
		body[size-1] = '\n'
	}
	return &Server{cfg: cfg, start: time.Now(), body: body, rng: rand.New(rand.NewPCG(cfg.Seed, 0x5eed))}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		p := s.peak.Load()
		if cur <= p || s.peak.CompareAndSwap(p, cur) {
			break
		}
	}

	delay := s.cfg.Latency
	if q := r.URL.Query().Get("latency"); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil || d < 0 {
			http.Error(w, "bad latency", http.StatusBadRequest)
			return
		}
		delay = d
	}
	status := http.StatusOK
	s.mu.Lock()
	if s.cfg.Jitter > 0 {
		delay += time.Duration(s.rng.Int64N(int64(s.cfg.Jitter)))
	}
	if s.cfg.ErrorRate > 0 && s.rng.Float64() < s.cfg.ErrorRate {
		status = http.StatusInternalServerError
	}
	s.mu.Unlock()
	if q := r.URL.Query().Get("status"); q != "" {
		code, err := strconv.Atoi(q)
		if err != nil || code < 200 || code > 599 {
			http.Error(w, "bad status", http.StatusBadRequest)
			return
		}
		status = code
	}

	if !sleep(r.Context(), s.StallRemaining(time.Now())+delay) {
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.body)))
	w.WriteHeader(status)
	_, _ = w.Write(s.body)
}

// StallRemaining returns how long a request arriving at now must wait for
// the current stall window to end (0 outside a window).
func (s *Server) StallRemaining(now time.Time) time.Duration {
	if s.cfg.StallEvery <= 0 || s.cfg.StallFor <= 0 {
		return 0
	}
	phase := now.Sub(s.start) % s.cfg.StallEvery
	if phase >= s.cfg.StallEvery-s.cfg.StallFor {
		return s.cfg.StallEvery - phase
	}
	return 0
}

// Requests is the number of requests received.
func (s *Server) Requests() int64 { return s.requests.Load() }

// PeakInFlight is the highest number of concurrently handled requests.
func (s *Server) PeakInFlight() int64 { return s.peak.Load() }

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
