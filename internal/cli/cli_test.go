package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/demo"
)

func target(t *testing.T, cfg demo.Config) (*httptest.Server, *demo.Server) {
	t.Helper()
	d := demo.New(cfg)
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return srv, d
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestOpenModelWritesReports(t *testing.T) {
	t.Parallel()
	srv, d := target(t, demo.Config{Latency: time.Millisecond})
	dir := t.TempDir()
	jsonPath, mdPath := filepath.Join(dir, "r.json"), filepath.Join(dir, "r.md")

	code, out, errOut := runCLI("--rate", "100", "--duration", "500ms", "--quiet",
		"--fail-if", "p99>5s", "--fail-if", "error_rate>1%",
		"--out-json", jsonPath, "--out-md", mdPath, srv.URL+"/hello")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if d.Requests() != 50 || !strings.Contains(out, "sent 50") {
		t.Fatalf("server saw %d requests; stdout:\n%s", d.Requests(), out)
	}
	var rep struct {
		Config struct {
			Model   string   `json:"model"`
			Targets []string `json:"targets"`
		} `json:"config"`
		Requests struct {
			Sent, Completed, OK uint64
		} `json:"requests"`
		StatusCodes map[string]uint64 `json:"status_codes"`
		Latency     struct {
			Basis string  `json:"basis"`
			P99   float64 `json:"p99_ms"`
		} `json:"latency"`
		Thresholds []struct {
			Breached bool `json:"breached"`
		} `json:"thresholds"`
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Config.Model != "open" || rep.Requests.Completed != 50 || rep.Requests.OK != 50 || rep.StatusCodes["200"] != 50 ||
		rep.Latency.Basis != "intended_time" || rep.Latency.P99 <= 0 || len(rep.Thresholds) != 2 || rep.Thresholds[0].Breached {
		t.Fatalf("unexpected report: %+v", rep)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Load test report", "| latency (from intended send time) | 50 |", "| `p99>5s` |", "| 200 | 50 |"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestLatencyThresholdBreachExitsTwo(t *testing.T) {
	t.Parallel()
	srv, _ := target(t, demo.Config{Latency: 20 * time.Millisecond})
	code, out, _ := runCLI("--rate", "50", "--duration", "300ms", "--quiet", "--fail-if", "p50>5ms", srv.URL)
	if code != ExitBreached || !strings.Contains(out, "threshold FAIL  p50>5ms") {
		t.Fatalf("exit %d, stdout:\n%s", code, out)
	}
}

func TestErrorRateThresholdAndExpectStatus(t *testing.T) {
	t.Parallel()
	srv, _ := target(t, demo.Config{ErrorRate: 1})
	code, out, _ := runCLI("--rate", "50", "--duration", "200ms", "--quiet", "--fail-if", "error_rate>1%", srv.URL)
	if code != ExitBreached || !strings.Contains(out, "status: 500=10") || !strings.Contains(out, "failed 10 (100.00%)") {
		t.Fatalf("exit %d, stdout:\n%s", code, out)
	}
	// The same responses pass when 500 is declared acceptable.
	code, out, _ = runCLI("--rate", "50", "--duration", "200ms", "--quiet", "--expect-status", "200-299,500", "--fail-if", "error_rate>1%", srv.URL)
	if code != ExitOK {
		t.Fatalf("exit %d, stdout:\n%s", code, out)
	}
}

func TestClosedModelWithFlagsAfterURL(t *testing.T) {
	t.Parallel()
	srv, d := target(t, demo.Config{Latency: 5 * time.Millisecond})
	code, out, errOut := runCLI(srv.URL, "--vus", "3", "--duration", "300ms", "--think", "10ms", "--out-json", "-")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout should be JSON when --out-json=-: %v\n%s", err, out)
	}
	if rep["config"].(map[string]any)["model"] != "closed" || d.PeakInFlight() > 3 {
		t.Fatalf("model %v peak %d", rep["config"], d.PeakInFlight())
	}
	if !strings.Contains(errOut, "closed model") || !strings.Contains(errOut, "warning: closed model") {
		t.Fatalf("stderr should carry the banner and text summary:\n%s", errOut)
	}
}

func TestRequestsFileWithTemplates(t *testing.T) {
	t.Parallel()
	srv, d := target(t, demo.Config{})
	file := filepath.Join(t.TempDir(), "reqs.json")
	body := `[{"method":"POST","url":"` + srv.URL + `/items/{{seq}}","body":"{\"id\":{{seq}}}","weight":3},
	          {"url":"` + srv.URL + `/missing?status=404"}]`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("--requests", file, "--stage", "200ms:40rps", "--start", "40", "--quiet")
	if code != ExitOK || d.Requests() != 8 {
		t.Fatalf("exit %d, requests %d\n%s", code, d.Requests(), out)
	}
	// Weighted round-robin 3:1 over 8 requests -> 6 x 200, 2 x 404 (404 counts as failed).
	if !strings.Contains(out, "status: 200=6 404=2") || !strings.Contains(out, "failed 2") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"no url":             {"--rate", "10"},
		"no model":           {"http://x.test/"},
		"rate and vus":       {"--rate", "10", "--vus", "2", "http://x.test/"},
		"stage and rate":     {"--stage", "1s:10", "--rate", "5", "http://x.test/"},
		"mixed stage units":  {"--stage", "1s:10rps", "--stage", "1s:2vu", "http://x.test/"},
		"bad stage":          {"--stage", "fast:10", "http://x.test/"},
		"bad threshold":      {"--rate", "1", "--fail-if", "p99>250", "http://x.test/"},
		"think on open":      {"--rate", "1", "--think", "1s", "http://x.test/"},
		"inflight on closed": {"--vus", "1", "--max-inflight", "5", "http://x.test/"},
		"bad url":            {"--rate", "1", "localhost:8080"},
		"two urls":           {"--rate", "1", "http://a.test/", "http://b.test/"},
		"unknown flag":       {"--frobnicate"},
		"bad status set":     {"--rate", "1", "--expect-status", "2xx", "http://x.test/"},
		"bad sig digits":     {"--rate", "1", "--sig-digits", "9", "http://x.test/"},
	}
	for name, args := range cases {
		if code, _, errOut := runCLI(args...); code != ExitError || errOut == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
}

func TestVersionAndHelp(t *testing.T) {
	t.Parallel()
	if code, out, _ := runCLI("--version"); code != ExitOK || !strings.Contains(out, Version) {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errOut := runCLI("--help"); code != ExitOK || !strings.Contains(errOut, "coordinated-omission") {
		t.Fatalf("help: %d %q", code, errOut)
	}
}
