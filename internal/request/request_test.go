package request

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func mustCompile(t *testing.T, ts ...Template) *Set {
	t.Helper()
	s, err := Compile(ts, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPlaceholdersRender(t *testing.T) {
	s := mustCompile(t, Template{
		Method:  "post",
		URL:     "http://example.test/items/{{seq}}?t={{unix}}",
		Headers: map[string]string{"X-Request-Id": "{{uuid}}", "Host": "api.example.test"},
		Body:    `{"n":{{ seq }},"ms":{{unix_ms}}}`,
	})
	now := time.Unix(1700000000, 123_000_000)
	req, err := s.Build(context.Background(), 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.String() != "http://example.test/items/42?t=1700000000" {
		t.Fatalf("got %s %s", req.Method, req.URL)
	}
	if !uuidRE.MatchString(req.Header.Get("X-Request-Id")) {
		t.Fatalf("bad uuid %q", req.Header.Get("X-Request-Id"))
	}
	if req.Host != "api.example.test" {
		t.Fatalf("host %q", req.Host)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != `{"n":42,"ms":1700000000123}` || req.ContentLength != int64(len(body)) {
		t.Fatalf("body %q len %d", body, req.ContentLength)
	}
}

func TestUUIDsDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		u := newUUID()
		if seen[u] || !uuidRE.MatchString(u) {
			t.Fatalf("duplicate or malformed uuid %q", u)
		}
		seen[u] = true
	}
}

func TestEnvPlaceholderResolvedAtCompile(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "TOKEN" {
			return "s3cret", true
		}
		return "", false
	}
	s, err := Compile([]Template{{URL: "http://x.test/", Headers: map[string]string{"Authorization": "Bearer {{env:TOKEN}}"}}}, env)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := s.Build(context.Background(), 0, time.Now())
	if req.Header.Get("Authorization") != "Bearer s3cret" {
		t.Fatalf("got %q", req.Header.Get("Authorization"))
	}
	if _, err := Compile([]Template{{URL: "http://x.test/{{env:MISSING}}"}}, env); err == nil {
		t.Fatal("missing env var should fail")
	}
}

func TestCompileErrors(t *testing.T) {
	bad := []Template{
		{URL: "http://x.test/{{nope}}"},
		{URL: "http://x.test/{{seq"},
		{URL: "ftp://x.test/"},
		{URL: "/relative"},
		{URL: "http://x.test/", Weight: -1},
		{URL: "http://x.test/", Headers: map[string]string{"Bad Name": "v"}},
	}
	for _, tpl := range bad {
		if _, err := Compile([]Template{tpl}, noEnv); err == nil {
			t.Errorf("%+v: expected error", tpl)
		}
	}
	if _, err := Compile(nil, noEnv); err == nil {
		t.Error("empty template list should fail")
	}
}

func TestWeightedRoundRobinIsDeterministic(t *testing.T) {
	s := mustCompile(t,
		Template{URL: "http://x.test/a", Weight: 1},
		Template{URL: "http://x.test/b", Weight: 3},
		Template{URL: "http://x.test/c"}, // default weight 1
	)
	counts := make([]int, 3)
	for seq := range uint64(500) {
		counts[s.Pick(seq)]++
	}
	if counts[0] != 100 || counts[1] != 300 || counts[2] != 100 {
		t.Fatalf("counts %v", counts)
	}
	if got := s.Describe(); got[1] != "GET http://x.test/b" {
		t.Fatalf("describe %v", got)
	}
}

func TestLoadFileObjectAndArray(t *testing.T) {
	dir := t.TempDir()
	obj := filepath.Join(dir, "one.json")
	arr := filepath.Join(dir, "many.json")
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(obj, []byte(`{"method":"PUT","url":"http://x.test/k/{{seq}}","body":"v"}`), 0o600)
	_ = os.WriteFile(arr, []byte(`[{"url":"http://x.test/a","weight":2},{"url":"http://x.test/b"}]`), 0o600)
	_ = os.WriteFile(bad, []byte(`{"url":"http://x.test/","verb":"GET"}`), 0o600)

	ts, err := LoadFile(obj)
	if err != nil || len(ts) != 1 || ts[0].Method != "PUT" {
		t.Fatalf("object: %v %+v", err, ts)
	}
	ts, err = LoadFile(arr)
	if err != nil || len(ts) != 2 || ts[0].Weight != 2 {
		t.Fatalf("array: %v %+v", err, ts)
	}
	if _, err := LoadFile(bad); err == nil || !strings.Contains(err.Error(), "verb") {
		t.Fatalf("unknown field should be rejected, got %v", err)
	}
}

func TestParseHeader(t *testing.T) {
	n, v, err := ParseHeader("Content-Type:  application/json ")
	if err != nil || n != "Content-Type" || v != "application/json" {
		t.Fatalf("%q %q %v", n, v, err)
	}
	if _, _, err := ParseHeader("novalue"); err == nil {
		t.Fatal("expected error")
	}
}
