// Package request compiles request templates and builds *http.Request values.
//
// URL, header values and body may contain placeholders:
//
//	{{seq}}       global 0-based request sequence number
//	{{uuid}}      random version-4 UUID
//	{{unix}}      current Unix time in seconds
//	{{unix_ms}}   current Unix time in milliseconds
//	{{env:NAME}}  environment variable NAME, resolved once at start-up
//
// Templates are compiled once into literal and placeholder parts so that
// building a request does not parse anything on the hot path.
package request

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Template describes one kind of request. Weight controls how often it is
// chosen relative to the other templates (default 1).
type Template struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Weight  int               `json:"weight,omitempty"`
}

type kind uint8

const (
	kLit kind = iota
	kSeq
	kUUID
	kUnix
	kUnixMS
)

type part struct {
	kind kind
	lit  string
}

// text is a compiled template string.
type text []part

func compileText(s string, lookupEnv func(string) (string, bool)) (text, error) {
	var out text
	add := func(p part) {
		if p.kind == kLit && len(out) > 0 && out[len(out)-1].kind == kLit {
			out[len(out)-1].lit += p.lit
			return
		}
		out = append(out, p)
	}
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if rest != "" {
				add(part{kind: kLit, lit: rest})
			}
			return out, nil
		}
		if i > 0 {
			add(part{kind: kLit, lit: rest[:i]})
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated placeholder in %q", s)
		}
		name := strings.TrimSpace(rest[i+2 : i+2+j])
		switch name {
		case "seq":
			add(part{kind: kSeq})
		case "uuid":
			add(part{kind: kUUID})
		case "unix":
			add(part{kind: kUnix})
		case "unix_ms":
			add(part{kind: kUnixMS})
		default:
			env, ok := strings.CutPrefix(name, "env:")
			if !ok {
				return nil, fmt.Errorf("unknown placeholder {{%s}} in %q", name, s)
			}
			v, found := lookupEnv(env)
			if !found {
				return nil, fmt.Errorf("placeholder {{env:%s}}: environment variable is not set", env)
			}
			add(part{kind: kLit, lit: v})
		}
		rest = rest[i+2+j+2:]
	}
}

func (t text) static() (string, bool) {
	switch {
	case len(t) == 0:
		return "", true
	case len(t) == 1 && t[0].kind == kLit:
		return t[0].lit, true
	}
	return "", false
}

func (t text) render(seq uint64, now time.Time) string {
	if s, ok := t.static(); ok {
		return s
	}
	var b strings.Builder
	for _, p := range t {
		switch p.kind {
		case kLit:
			b.WriteString(p.lit)
		case kSeq:
			b.WriteString(strconv.FormatUint(seq, 10))
		case kUUID:
			b.WriteString(newUUID())
		case kUnix:
			b.WriteString(strconv.FormatInt(now.Unix(), 10))
		case kUnixMS:
			b.WriteString(strconv.FormatInt(now.UnixMilli(), 10))
		}
	}
	return b.String()
}

func newUUID() string {
	var u [16]byte
	hi, lo := rand.Uint64(), rand.Uint64()
	for i := range 8 {
		u[i] = byte(hi >> (56 - 8*i))
		u[8+i] = byte(lo >> (56 - 8*i))
	}
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}

type header struct {
	name  string
	value text
}

type compiled struct {
	method  string
	url     text
	headers []header
	body    text
}

// Set is a compiled, immutable collection of templates. It is safe for
// concurrent use.
type Set struct {
	items []compiled
	cum   []int // cumulative weights
	total int
	descr []string
}

// Compile validates and compiles templates. lookupEnv resolves {{env:NAME}}
// placeholders (pass os.LookupEnv in production).
func Compile(ts []Template, lookupEnv func(string) (string, bool)) (*Set, error) {
	if len(ts) == 0 {
		return nil, errors.New("request: no request templates")
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	s := &Set{}
	for i, t := range ts {
		c, err := compileOne(t, lookupEnv)
		if err != nil {
			return nil, fmt.Errorf("request template %d: %w", i+1, err)
		}
		w := t.Weight
		if w < 0 {
			return nil, fmt.Errorf("request template %d: weight must be >= 0", i+1)
		}
		if w == 0 {
			w = 1
		}
		s.total += w
		s.items = append(s.items, c)
		s.cum = append(s.cum, s.total)
		s.descr = append(s.descr, c.method+" "+t.URL)
	}
	return s, nil
}

func compileOne(t Template, lookupEnv func(string) (string, bool)) (compiled, error) {
	method := strings.ToUpper(strings.TrimSpace(t.Method))
	if method == "" {
		method = http.MethodGet
	}
	c := compiled{method: method}
	var err error
	if c.url, err = compileText(t.URL, lookupEnv); err != nil {
		return c, err
	}
	u, err := url.Parse(c.url.render(0, time.Unix(0, 0)))
	if err != nil {
		return c, fmt.Errorf("invalid url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("url %q must be absolute http:// or https://", t.URL)
	}
	if c.body, err = compileText(t.Body, lookupEnv); err != nil {
		return c, err
	}
	names := make([]string, 0, len(t.Headers))
	for name := range t.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, " :\r\n") {
			return c, fmt.Errorf("invalid header name %q", name)
		}
		v, err := compileText(t.Headers[name], lookupEnv)
		if err != nil {
			return c, err
		}
		c.headers = append(c.headers, header{name: http.CanonicalHeaderKey(name), value: v})
	}
	return c, nil
}

// Describe returns "METHOD URL" for each template (placeholders unexpanded).
func (s *Set) Describe() []string { return slices.Clone(s.descr) }

// Pick returns the template index used for request seq. Selection is a
// deterministic weighted round-robin over the sequence number.
func (s *Set) Pick(seq uint64) int {
	if len(s.items) == 1 {
		return 0
	}
	r := int(seq % uint64(s.total))
	return sort.SearchInts(s.cum, r+1)
}

// Build creates the request with sequence number seq.
func (s *Set) Build(ctx context.Context, seq uint64, now time.Time) (*http.Request, error) {
	c := &s.items[s.Pick(seq)]
	var body io.Reader
	if len(c.body) > 0 {
		body = strings.NewReader(c.body.render(seq, now))
	}
	req, err := http.NewRequestWithContext(ctx, c.method, c.url.render(seq, now), body)
	if err != nil {
		return nil, err
	}
	for _, h := range c.headers {
		v := h.value.render(seq, now)
		if h.name == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(h.name, v)
	}
	return req, nil
}

// LoadFile reads templates from a JSON file containing either one template
// object or an array of them.
func LoadFile(path string) ([]Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var ts []Template
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var t Template
		err = dec.Decode(&t)
		ts = []Template{t}
	} else {
		err = dec.Decode(&ts)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ts, nil
}

// ParseHeader parses a curl-style "Name: value" header.
func ParseHeader(s string) (name, value string, err error) {
	name, value, ok := strings.Cut(s, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return "", "", fmt.Errorf("header %q: want \"Name: value\"", s)
	}
	return name, strings.TrimSpace(value), nil
}
