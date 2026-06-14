//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// runPlatform runs the application as a foreground process until it receives
// an interrupt or termination signal.
func runPlatform(_ string, run func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}
