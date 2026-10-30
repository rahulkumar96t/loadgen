// Package cli implements the loadgen command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/report"
	"github.com/rahulkumar96t/loadgen/internal/request"
	"github.com/rahulkumar96t/loadgen/internal/runner"
	"github.com/rahulkumar96t/loadgen/internal/schedule"
	"github.com/rahulkumar96t/loadgen/internal/stats"
	"github.com/rahulkumar96t/loadgen/internal/threshold"
)

// Version of the tool.
const Version = "0.1.0"

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1 // bad usage or the run could not start
	ExitBreached = 2 // at least one --fail-if threshold was breached
)

const usage = `loadgen - HTTP load generator with coordinated-omission-free latency

Usage:
  loadgen [flags] URL
  loadgen [flags] --requests requests.json

Open model (default): requests arrive on a schedule, latency is measured from
the intended send time.
  loadgen --rate 200 --duration 30s http://localhost:8080/
  loadgen --stage 10s:200rps --stage 30s:200rps --stage 5s:0rps http://localhost:8080/

Closed model: N virtual users send, wait for the response, think, repeat.
  loadgen --vus 50 --duration 30s --think 100ms http://localhost:8080/
  loadgen --stage 10s:50vu --stage 30s:50vu http://localhost:8080/

CI gates (exit code 2 when breached):
  loadgen --rate 100 --duration 20s --fail-if 'p99>250ms' --fail-if 'error_rate>1%' URL

Flags:
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ", ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	method, body, bodyFile, requestsFile string
	headers, stages, failIf              multi
	rate, start                          float64
	vus, maxInFlight, digits             int
	duration, think, timeout             time.Duration
	expect, outJSON, outMD               string
	quiet, insecure, noKeepAlive, ver    bool
	url                                  string
	set                                  map[string]bool
}

func parse(args []string, stderr io.Writer) (*options, error) {
	o := &options{set: map[string]bool{}}
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.method, "X", "GET", "HTTP method (alias --method)")
	fs.StringVar(&o.method, "method", "GET", "HTTP method")
	fs.Var(&o.headers, "H", "request header 'Name: value', repeatable (alias --header)")
	fs.Var(&o.headers, "header", "request header 'Name: value', repeatable")
	fs.StringVar(&o.body, "d", "", "request body (alias --body)")
	fs.StringVar(&o.body, "body", "", "request body")
	fs.StringVar(&o.bodyFile, "body-file", "", "read the request body from a file")
	fs.StringVar(&o.requestsFile, "requests", "", "JSON file with one request template or an array of weighted templates")
	fs.Float64Var(&o.rate, "rate", 0, "open model: constant arrival rate in requests/second")
	fs.IntVar(&o.vus, "vus", 0, "closed model: constant number of virtual users")
	fs.DurationVar(&o.duration, "duration", 10*time.Second, "test duration for --rate or --vus")
	fs.Var(&o.stages, "stage", "ramp stage DURATION:TARGET[rps|vu], repeatable; each stage ramps linearly to its target")
	fs.Float64Var(&o.start, "start", 0, "value (rps or VUs) the first stage ramps from")
	fs.IntVar(&o.maxInFlight, "max-inflight", 1000, "open model: maximum concurrent requests (0 = unlimited)")
	fs.DurationVar(&o.think, "think", 0, "closed model: pause between a response and the next request")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.StringVar(&o.expect, "expect-status", "200-399", "status codes counted as success, e.g. 200-299,404")
	fs.Var(&o.failIf, "fail-if", "failure threshold such as 'p99>250ms' or 'error_rate>1%', repeatable")
	fs.StringVar(&o.outJSON, "out-json", "", "write the JSON report to this file ('-' for stdout)")
	fs.StringVar(&o.outMD, "out-md", "", "write the Markdown report to this file ('-' for stdout)")
	fs.IntVar(&o.digits, "sig-digits", 3, "histogram precision in significant decimal digits (1-5)")
	fs.BoolVar(&o.quiet, "quiet", false, "no progress output on stderr")
	fs.BoolVar(&o.insecure, "insecure", false, "skip TLS certificate verification")
	fs.BoolVar(&o.noKeepAlive, "no-keepalive", false, "open a new connection for every request")
	fs.BoolVar(&o.ver, "version", false, "print the version and exit")

	// Allow flags after the URL: parse, take one positional, repeat.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("expected one URL, got %d arguments", len(positional))
	}
	if len(positional) == 1 {
		o.url = positional[0]
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return o, nil
}

func (o *options) profile() (runner.Model, *schedule.Profile, error) {
	switch {
	case len(o.stages) > 0:
		if o.set["rate"] || o.set["vus"] || o.set["duration"] {
			return "", nil, errors.New("--stage cannot be combined with --rate, --vus or --duration")
		}
		model := runner.Open
		var sts []schedule.Stage
		var sawRPS, sawVU bool
		for _, s := range o.stages {
			st, unit, err := schedule.ParseStage(s)
			if err != nil {
				return "", nil, err
			}
			sawRPS = sawRPS || unit == schedule.UnitRPS
			sawVU = sawVU || unit == schedule.UnitVU
			sts = append(sts, st)
		}
		if sawRPS && sawVU {
			return "", nil, errors.New("stages mix rps and vu targets")
		}
		if sawVU {
			model = runner.Closed
		}
		p, err := schedule.NewProfile(o.start, sts...)
		return model, p, err
	case o.rate > 0 && o.vus > 0:
		return "", nil, errors.New("use either --rate (open model) or --vus (closed model), not both")
	case o.rate > 0:
		p, err := schedule.Constant(o.rate, o.duration)
		return runner.Open, p, err
	case o.vus > 0:
		p, err := schedule.Constant(float64(o.vus), o.duration)
		return runner.Closed, p, err
	}
	return "", nil, errors.New("one of --rate, --vus or --stage is required")
}

func (o *options) templates() ([]request.Template, error) {
	if o.requestsFile != "" {
		if o.url != "" || o.set["body"] || o.set["d"] || o.set["body-file"] || o.set["X"] || o.set["method"] || len(o.headers) > 0 {
			return nil, errors.New("--requests replaces the URL, method, headers and body flags")
		}
		return request.LoadFile(o.requestsFile)
	}
	if o.url == "" {
		return nil, errors.New("missing target URL")
	}
	body := o.body
	if o.bodyFile != "" {
		if body != "" {
			return nil, errors.New("use either --body or --body-file")
		}
		data, err := os.ReadFile(o.bodyFile)
		if err != nil {
			return nil, err
		}
		body = string(data)
	}
	headers := map[string]string{}
	for _, h := range o.headers {
		name, value, err := request.ParseHeader(h)
		if err != nil {
			return nil, err
		}
		if prev, ok := headers[name]; ok {
			value = prev + ", " + value
		}
		headers[name] = value
	}
	return []request.Template{{Method: o.method, URL: o.url, Headers: headers, Body: body}}, nil
}

// Run executes the command and returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "loadgen:", err)
		return ExitError
	}
	if o.ver {
		fmt.Fprintln(stdout, "loadgen", Version)
		return ExitOK
	}
	sum, err := run(ctx, o, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "loadgen:", err)
		return ExitError
	}
	if sum.Breached() {
		return ExitBreached
	}
	return ExitOK
}

func run(ctx context.Context, o *options, stdout, stderr io.Writer) (*report.Summary, error) {
	model, profile, err := o.profile()
	if err != nil {
		return nil, err
	}
	if model == runner.Open && o.set["think"] {
		return nil, errors.New("--think only applies to the closed model")
	}
	if model == runner.Closed && o.set["max-inflight"] {
		return nil, errors.New("--max-inflight only applies to the open model (the VU count bounds concurrency)")
	}
	ts, err := o.templates()
	if err != nil {
		return nil, err
	}
	set, err := request.Compile(ts, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	expect, err := stats.ParseStatusSet(o.expect)
	if err != nil {
		return nil, err
	}
	var gates []threshold.Threshold
	for _, e := range o.failIf {
		th, err := threshold.Parse(e)
		if err != nil {
			return nil, err
		}
		gates = append(gates, th)
	}
	if o.digits < 1 || o.digits > 5 {
		return nil, errors.New("--sig-digits must be between 1 and 5")
	}
	if o.outJSON == "-" && o.outMD == "-" {
		return nil, errors.New("only one of --out-json and --out-md can write to stdout")
	}

	maxInFlight := o.maxInFlight
	conns := maxInFlight
	unit := "rps"
	if model == runner.Closed {
		maxInFlight, unit = 0, "VUs"
		conns = int(profile.Max() + 0.5)
	}
	client := runner.NewHTTPClient(runner.ClientOptions{Timeout: o.timeout, MaxConns: conns, Insecure: o.insecure, DisableKeepAlive: o.noKeepAlive})
	r, err := runner.New(runner.Config{
		Model: model, Profile: profile, Requests: set, Expect: expect, Digits: o.digits,
		MaxInFlight: maxInFlight, Think: o.think, Timeout: o.timeout, Client: client,
	})
	if err != nil {
		return nil, err
	}

	if !o.quiet {
		fmt.Fprintf(stderr, "loadgen: %s model, %s, %s\n", model, profile.Describe(unit), strings.Join(set.Describe(), "; "))
	}
	done := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		if o.quiet {
			return
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				p := r.Progress()
				fmt.Fprintf(stderr, "  t=%-6s target=%-8.1f sent=%-8d done=%-8d failed=%-6d in-flight=%-5d unsent=%d\n",
					p.Elapsed.Truncate(time.Second), p.Target, p.Sent, p.Completed, p.Failed, p.InFlight, p.Unsent)
			}
		}
	}()
	res := r.Run(ctx)
	close(done)
	<-progressDone

	rc := report.RunConfig{
		Model: string(model), Targets: set.Describe(), Profile: profile.Describe(unit),
		MaxInFlight: maxInFlight, Timeout: o.timeout.String(), ExpectStatus: expect.String(), SigDigits: o.digits,
	}
	if o.think > 0 {
		rc.Think = o.think.String()
	}
	sum := report.Build(res, rc, Version)
	if sum.Thresholds, err = threshold.Evaluate(gates, sum); err != nil {
		return nil, err
	}

	textToStdout := true
	for _, out := range []struct {
		path  string
		write func(io.Writer) error
	}{{o.outJSON, sum.WriteJSON}, {o.outMD, sum.WriteMarkdown}} {
		switch out.path {
		case "":
		case "-":
			textToStdout = false
			if err := out.write(stdout); err != nil {
				return nil, err
			}
		default:
			if err := writeFile(out.path, out.write); err != nil {
				return nil, err
			}
		}
	}
	if textToStdout {
		if err := sum.WriteText(stdout); err != nil {
			return nil, err
		}
	} else if !o.quiet {
		_ = sum.WriteText(stderr)
	}
	return sum, nil
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
