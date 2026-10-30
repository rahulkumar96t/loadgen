package demo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	bad := []Config{
		{Latency: -1},
		{ErrorRate: 1.5},
		{StallEvery: time.Second, StallFor: time.Second},
		{BodySize: -1},
	}
	for _, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%+v: expected error", c)
		}
	}
	if err := (Config{Latency: time.Millisecond, StallEvery: time.Second, StallFor: 100 * time.Millisecond}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStallWindowIsAtEndOfPeriod(t *testing.T) {
	s := New(Config{StallEvery: time.Second, StallFor: 300 * time.Millisecond})
	ms := time.Millisecond
	cases := map[time.Duration]time.Duration{
		0:          0,
		699 * ms:   0,
		700 * ms:   300 * ms,
		900 * ms:   100 * ms,
		1000 * ms:  0,
		1750 * ms:  250 * ms,
		10100 * ms: 0,
	}
	for at, want := range cases {
		if got := s.StallRemaining(s.start.Add(at)); got != want {
			t.Errorf("at %v: wait %v, want %v", at, got, want)
		}
	}
}

func TestErrorRateAndOverrides(t *testing.T) {
	s := New(Config{ErrorRate: 1, BodySize: 10})
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || len(body) != 10 {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}

	start := time.Now()
	resp, err = http.Get(srv.URL + "/?status=204&latency=30ms")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
	}

	resp, _ = http.Get(srv.URL + "/?latency=banana")
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad override should be 400, got %d", resp.StatusCode)
	}
	if s.Requests() != 3 || s.PeakInFlight() != 1 {
		t.Fatalf("requests %d peak %d", s.Requests(), s.PeakInFlight())
	}
}
