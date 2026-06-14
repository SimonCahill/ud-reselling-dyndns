//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/svc"
)

// runPlatform runs interactively from a console or registers the application
// loop with the Windows Service Control Manager when launched as a service.
func runPlatform(serviceName string, run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service session: %w", err)
	}
	if !isService {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return run(ctx)
	}

	return svc.Run(serviceName, &serviceHandler{run: run})
}

type serviceHandler struct {
	run func(context.Context) error
}

// Execute implements svc.Handler and translates SCM controls into context
// cancellation for the shared application loop.
func (handler *serviceHandler) Execute(
	_ []string,
	changes <-chan svc.ChangeRequest,
	statuses chan<- svc.Status,
) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	statuses <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- handler.run(ctx)
	}()

	current := svc.Status{State: svc.Running, Accepts: accepted}
	statuses <- current

	for {
		select {
		case err := <-done:
			statuses <- svc.Status{State: svc.StopPending}
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("Service stopped with an error: %v", err)
				return false, 1
			}
			return false, 0

		case change := <-changes:
			switch change.Cmd {
			case svc.Interrogate:
				statuses <- current
			case svc.Stop, svc.Shutdown:
				current = svc.Status{State: svc.StopPending}
				statuses <- current
				cancel()
			default:
				log.Printf("Ignoring unsupported Windows service control request: %d", change.Cmd)
			}
		}
	}
}
