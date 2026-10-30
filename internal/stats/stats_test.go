package stats

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestParseStatusSet(t *testing.T) {
	s, err := ParseStatusSet("200-299, 304,404")
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]bool{200: true, 299: true, 304: true, 404: true, 300: false, 500: false} {
		if s.Contains(code) != want {
			t.Errorf("Contains(%d) = %v", code, !want)
		}
	}
	if s.String() != "200-299,304,404" {
		t.Errorf("String() = %q", s.String())
	}
	for _, bad := range []string{"", "abc", "300-200", "99", "200-700", ","} {
		if _, err := ParseStatusSet(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyError(t *testing.T) {
	opErr := func(e error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", e)}
	}
	cases := []struct {
		want string
		err  error
	}{
		{"", nil},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled)},
		{"timeout", context.DeadlineExceeded},
		{"timeout", &net.OpError{Op: "read", Err: timeoutErr{}}},
		{"connection_refused", opErr(syscall.ECONNREFUSED)},
		{"connection_reset", opErr(syscall.ECONNRESET)},
		{"too_many_open_files", opErr(syscall.EMFILE)},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid"}},
		{"eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF)},
		{"other", errors.New("boom")},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestClassifyRealConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = http.Get("http://" + addr + "/")
	if got := ClassifyError(err); got != "connection_refused" {
		t.Fatalf("got %q for %v", got, err)
	}
}

func TestRecorderOpenModel(t *testing.T) {
	r, err := NewRecorder(true, 3, DefaultStatusSet)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1000, 0)
	ms := time.Millisecond
	// on time, 10 ms service
	r.Record(Outcome{Intended: base, Start: base, End: base.Add(10 * ms), Status: 200, Bytes: 5})
	// sent 90 ms late, 10 ms service -> 100 ms latency
	r.Record(Outcome{Intended: base, Start: base.Add(90 * ms), End: base.Add(100 * ms), Status: 503})
	// transport error
	r.Record(Outcome{Intended: base, Start: base, End: base.Add(ms), Err: context.DeadlineExceeded})

	res := r.Result()
	if res.Completed != 3 || res.Failed != 2 || res.BytesIn != 5 || res.Model != "open" {
		t.Fatalf("%+v", res)
	}
	if res.Statuses[200] != 1 || res.Statuses[503] != 1 || res.Errors["timeout"] != 1 {
		t.Fatalf("statuses %v errors %v", res.Statuses, res.Errors)
	}
	if got := time.Duration(res.Latency.Max()); got != 100*ms {
		t.Fatalf("latency max %v", got)
	}
	if got := time.Duration(res.Service.Max()); got != 10*ms {
		t.Fatalf("service max %v", got)
	}
	if got := time.Duration(res.Lag.Max()); got != 90*ms {
		t.Fatalf("lag max %v", got)
	}
}

func TestRecorderClosedModelSharesHistogram(t *testing.T) {
	r, _ := NewRecorder(false, 2, DefaultStatusSet)
	now := time.Now()
	r.Record(Outcome{Start: now, End: now.Add(time.Millisecond), Status: 204})
	res := r.Result()
	if res.Latency != res.Service || res.Lag != nil || res.Model != "closed" || res.Latency.Count() != 1 {
		t.Fatalf("%+v", res)
	}
	if c, f := r.Counts(); c != 1 || f != 0 {
		t.Fatalf("counts %d %d", c, f)
	}
}
