package runner

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/demo"
	"github.com/rahulkumar96t/loadgen/internal/request"
	"github.com/rahulkumar96t/loadgen/internal/schedule"
	"github.com/rahulkumar96t/loadgen/internal/stats"
)

func requests(t *testing.T, url string) *request.Set {
	t.Helper()
	s, err := request.Compile([]request.Template{{URL: url}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustRun(t *testing.T, cfg Config) *stats.Result {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r.Run(context.Background())
}

func ms(d int64) time.Duration { return time.Duration(d) * time.Millisecond }

// fakeClock advances only when the dispatcher sleeps, so a long profile runs
// instantly and every intended send time can be observed exactly.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) SleepUntil(ctx context.Context, t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
	c.sleeps = append(c.sleeps, t)
	return ctx.Err()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenModelFollowsScheduleInVirtualTime(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	// 0 -> 500 rps over 10 s, then 500 rps for 10 s: 2500 + 5000 arrivals.
	profile, _ := schedule.NewProfile(0, schedule.Stage{Duration: 10 * time.Second, Target: 500}, schedule.Stage{Duration: 10 * time.Second, Target: 500})
	res := mustRun(t, Config{Model: Open, Profile: profile, Requests: requests(t, "http://target.test/"), Client: client, Clock: clock})

	if res.Scheduled != 7500 || res.Sent != 7500 || res.Completed != 7500 || calls.Load() != 7500 {
		t.Fatalf("scheduled=%d sent=%d completed=%d calls=%d", res.Scheduled, res.Sent, res.Completed, calls.Load())
	}
	start := time.Unix(1_000_000, 0)
	perSecond := make([]int, 20)
	for _, s := range clock.sleeps {
		perSecond[int(s.Sub(start)/time.Second)]++
	}
	for sec, got := range perSecond {
		want := 500 // hold phase
		if sec < 10 {
			want = 25 * (2*sec + 1) // integral of 50t over [sec, sec+1)
		}
		if got < want-1 || got > want+1 {
			t.Errorf("second %d: %d sends, want %d±1", sec, got, want)
		}
	}
}

func TestOpenModelRateAccuracyRealTime(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	target := demo.New(demo.Config{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		target.ServeHTTP(w, r)
	}))
	defer srv.Close()

	profile, _ := schedule.Constant(200, time.Second)
	res := mustRun(t, Config{Model: Open, Profile: profile, Requests: requests(t, srv.URL)})

	if res.Scheduled != 200 || res.Sent != 200 || res.Completed != 200 || res.Failed != 0 || res.Unsent != 0 {
		t.Fatalf("scheduled=%d sent=%d completed=%d failed=%d unsent=%d", res.Scheduled, res.Sent, res.Completed, res.Failed, res.Unsent)
	}
	if res.Elapsed < ms(990) || res.Elapsed > 3*time.Second {
		t.Fatalf("elapsed %v", res.Elapsed)
	}
	firstHalf := 0
	for _, a := range arrivals {
		if a.Sub(res.Started) < ms(500) {
			firstHalf++
		}
	}
	// 100 requests are due in the first 500 ms; allow for scheduler jitter on busy CI hosts.
	if firstHalf < 80 || firstHalf > 101 {
		t.Fatalf("%d requests arrived in the first 500 ms, want ~100", firstHalf)
	}
	if lag := time.Duration(res.Lag.ValueAtQuantile(0.9)); lag > ms(100) {
		t.Fatalf("send lag p90 %v: generator fell behind a 200 rps schedule", lag)
	}
}

// TestCoordinatedOmissionIsCorrected runs a single-connection open-model test
// against a server that freezes for 500 ms. Service time only sees the one
// request that hit the freeze; intended-time latency also charges the ~50
// requests that were due during the freeze and had to wait.
func TestCoordinatedOmissionIsCorrected(t *testing.T) {
	target := demo.New(demo.Config{Latency: time.Millisecond, StallEvery: ms(1500), StallFor: ms(500)})
	srv := httptest.NewServer(target)
	defer srv.Close()

	profile, _ := schedule.Constant(100, 2*time.Second)
	res := mustRun(t, Config{Model: Open, Profile: profile, Requests: requests(t, srv.URL), MaxInFlight: 1})

	if res.Completed < 190 || res.Failed != 0 {
		t.Fatalf("completed=%d failed=%d unsent=%d", res.Completed, res.Failed, res.Unsent)
	}
	intendedP90 := time.Duration(res.Latency.ValueAtQuantile(0.9))
	serviceP90 := time.Duration(res.Service.ValueAtQuantile(0.9))
	serviceMax := time.Duration(res.Service.Max())
	t.Logf("intended p90=%v service p90=%v service max=%v", intendedP90, serviceP90, serviceMax)
	if serviceMax < ms(300) {
		t.Fatalf("the request that hit the stall should take ~500 ms, got %v", serviceMax)
	}
	if serviceP90 > ms(100) {
		t.Fatalf("service-time p90 %v should be unaffected by a single stall", serviceP90)
	}
	if intendedP90 < ms(150) {
		t.Fatalf("intended-time p90 %v should expose the stall", intendedP90)
	}
	if lag := time.Duration(res.Lag.Max()); lag < ms(300) {
		t.Fatalf("send lag max %v: requests due during the stall must have been sent late", lag)
	}
}

func TestUnsentWhenInFlightLimitIsReached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer
	}))
	defer srv.Close()

	profile, _ := schedule.Constant(100, ms(300))
	res := mustRun(t, Config{Model: Open, Profile: profile, Requests: requests(t, srv.URL), MaxInFlight: 1, Timeout: ms(500)})

	if res.Scheduled != 30 || res.Sent != 1 || res.Unsent != 29 {
		t.Fatalf("scheduled=%d sent=%d unsent=%d", res.Scheduled, res.Sent, res.Unsent)
	}
	if res.Completed != 1 || res.Errors["timeout"] != 1 || res.PeakConc != 1 {
		t.Fatalf("completed=%d errors=%v peak=%d", res.Completed, res.Errors, res.PeakConc)
	}
}

func TestTransportErrorsAreCounted(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	profile, _ := schedule.Constant(50, ms(200))
	res := mustRun(t, Config{Model: Open, Profile: profile, Requests: requests(t, "http://"+addr+"/")})
	if res.Completed != 10 || res.Failed != 10 || res.Errors["connection_refused"] != 10 {
		t.Fatalf("completed=%d failed=%d errors=%v", res.Completed, res.Failed, res.Errors)
	}
}

func TestClosedModelHoldsConcurrencyAtVUs(t *testing.T) {
	target := demo.New(demo.Config{Latency: 10 * time.Millisecond})
	srv := httptest.NewServer(target)
	defer srv.Close()

	profile, _ := schedule.Constant(4, ms(500))
	res := mustRun(t, Config{Model: Closed, Profile: profile, Requests: requests(t, srv.URL)})

	if target.PeakInFlight() != 4 || res.PeakConc != 4 {
		t.Fatalf("server peak %d, runner peak %d, want 4", target.PeakInFlight(), res.PeakConc)
	}
	// Each VU does at most one 10 ms request at a time for 500 ms.
	if res.Completed < 20 || res.Completed > 210 || res.Failed != 0 {
		t.Fatalf("completed=%d failed=%d", res.Completed, res.Failed)
	}
	if res.Latency != res.Service || res.Lag != nil || res.Model != "closed" {
		t.Fatal("closed model should report service time only")
	}
}

func TestClosedModelRampAndThinkTime(t *testing.T) {
	target := demo.New(demo.Config{})
	srv := httptest.NewServer(target)
	defer srv.Close()

	// Ramp 0 -> 3 VUs over 300 ms, hold 3 VUs for 300 ms, 50 ms think time.
	profile, _ := schedule.NewProfile(0, schedule.Stage{Duration: ms(300), Target: 3}, schedule.Stage{Duration: ms(300), Target: 3})
	res := mustRun(t, Config{Model: Closed, Profile: profile, Requests: requests(t, srv.URL), Think: ms(50)})

	if res.PeakConc != 3 || target.PeakInFlight() > 3 {
		t.Fatalf("runner peak %d server peak %d", res.PeakConc, target.PeakInFlight())
	}
	// With 50 ms think time, 3 VUs over <= 600 ms can send at most ~3*12 requests.
	if res.Completed < 5 || res.Completed > 40 {
		t.Fatalf("completed=%d", res.Completed)
	}
}

func TestRunStopsWhenContextIsCancelled(t *testing.T) {
	srv := httptest.NewServer(demo.New(demo.Config{}))
	defer srv.Close()
	profile, _ := schedule.Constant(50, time.Minute)
	r, err := New(Config{Model: Open, Profile: profile, Requests: requests(t, srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ms(200))
	defer cancel()
	begin := time.Now()
	res := r.Run(ctx)
	if time.Since(begin) > 5*time.Second || !res.Interrupted || res.Sent == 0 || res.Unsent != 0 {
		t.Fatalf("took %v interrupted=%v sent=%d unsent=%d", time.Since(begin), res.Interrupted, res.Sent, res.Unsent)
	}
	if p := r.Progress(); p.Sent != int64(res.Sent) || p.Target != 50 {
		t.Fatalf("progress %+v", p)
	}
}

func TestNewValidates(t *testing.T) {
	profile, _ := schedule.Constant(1, time.Second)
	set := requests(t, "http://x.test/")
	bad := []Config{
		{Model: "sideways", Profile: profile, Requests: set},
		{Model: Open, Requests: set},
		{Model: Open, Profile: profile, Requests: set, MaxInFlight: -1},
		{Model: Open, Profile: profile, Requests: set, Digits: 9},
	}
	for i, c := range bad {
		if _, err := New(c); err == nil {
			t.Errorf("config %d: expected error", i)
		}
	}
}
