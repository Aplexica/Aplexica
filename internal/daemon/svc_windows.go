//go:build windows

// Package daemon — Windows SCM glue. Built only on windows. The
// non-windows companion `svc_nonwindows.go` exposes the same
// IsWindowsService surface returning (false, nil) so callers in
// build-tag-free files (cmd/aplexica/cmd_daemon.go) compile and link
// on every platform.
package daemon

import (
	"context"
	"log/slog"

	"golang.org/x/sys/windows/svc"
)

// ServiceName is the canonical Windows service name registered by
// install_windows.go's Install + queried by Uninstall + passed to
// svc.Run by RunAsService.
const ServiceName = "Aplexica"

// IsWindowsService is a re-export of svc.IsWindowsService so callers
// can detect the SCM-launched context without importing
// x/sys/windows/svc directly. Returns (true, nil) when the process was
// started by Windows SCM; (false, nil) when running interactively.
func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

// RunAsService binds a foreground `daemon serve` body into SCM's
// request/response cycle. The handler is called in its own goroutine;
// the ctx it receives is canceled when SCM sends a Stop or Shutdown
// request. The handler MUST return promptly after ctx is canceled —
// RunAsService blocks reporting StopPending until it does, and SCM
// will eventually mark the service "hung" if reporting takes too long.
//
// Returns whatever error svc.Run returns. Handler errors are logged via
// lg.Error but not surfaced to SCM — SCM only cares about the service-
// specific exit code, which we always set to 0 (handler exited cleanly,
// even if it returned an error).
func RunAsService(lg *slog.Logger, handler func(ctx context.Context) error) error {
	h := &aplexicaService{handler: handler, lg: lg}
	return svc.Run(ServiceName, h)
}

type aplexicaService struct {
	handler func(ctx context.Context) error
	lg      *slog.Logger
}

// Execute satisfies svc.Handler. It bridges SCM's ChangeRequest channel
// to a cancelable context.Context the handler reads from. The
// transition sequence per Microsoft's service docs is:
//
//	StartPending -> Running -> StopPending -> Stopped
//
// We acknowledge Interrogate by echoing back the most recent status SCM
// last saw (svc.ChangeRequest carries it in CurrentStatus).
func (s *aplexicaService) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (ssec bool, errno uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- s.handler(ctx)
	}()
	status <- svc.Status{State: svc.Running, Accepts: accepts}

	var handlerErr error
loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.lg.Info("SCM requested stop", "cmd", c.Cmd)
				cancel()
				// Wait for the handler to drain via the done channel
				// below; the loop break happens once done fires.
			default:
				// Unsupported request; ignore (Interrogate aside, we
				// only declared AcceptStop|AcceptShutdown).
			}
		case handlerErr = <-done:
			if handlerErr != nil {
				s.lg.Error("daemon handler exited with error", "err", handlerErr)
			}
			break loop
		}
	}

	status <- svc.Status{State: svc.StopPending}
	status <- svc.Status{State: svc.Stopped}
	return false, 0
}
