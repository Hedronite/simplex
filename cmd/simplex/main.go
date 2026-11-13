// Command simplex probes a mesh from one vantage and reports structured
// health as a simplex.probe/v0 document.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, realDeps())
	stop()
	// main is the only place that exits the process.
	os.Exit(code)
}
