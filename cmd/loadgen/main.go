// Command loadgen is an HTTP load generator with open (constant arrival rate)
// and closed (virtual user) models, HDR-style latency histograms and CI
// thresholds.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rahulkumar96t/loadgen/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
