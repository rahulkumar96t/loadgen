// Command demoserver is a toy HTTP target with configurable latency, jitter,
// error rate and periodic stalls, for trying loadgen locally.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rahulkumar96t/loadgen/internal/demo"
)

func main() {
	var cfg demo.Config
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	flag.DurationVar(&cfg.Latency, "latency", 5*time.Millisecond, "base response latency")
	flag.DurationVar(&cfg.Jitter, "jitter", 0, "extra random latency in [0, jitter)")
	flag.Float64Var(&cfg.ErrorRate, "error-rate", 0, "fraction of requests answered with HTTP 500")
	flag.DurationVar(&cfg.StallEvery, "stall-every", 0, "period of simulated freezes (0 = never)")
	flag.DurationVar(&cfg.StallFor, "stall-for", 0, "length of each freeze, at the end of every period")
	flag.IntVar(&cfg.BodySize, "body-size", 0, "response body size in bytes (default 3)")
	flag.Uint64Var(&cfg.Seed, "seed", 1, "random seed")
	flag.Parse()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "demoserver:", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: demo.New(cfg), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("demoserver listening on http://%s (latency=%v jitter=%v error-rate=%v stall=%v every %v)",
		ln.Addr(), cfg.Latency, cfg.Jitter, cfg.ErrorRate, cfg.StallFor, cfg.StallEvery)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
