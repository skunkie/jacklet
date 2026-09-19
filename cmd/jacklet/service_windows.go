// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// serviceName is the name the Service Control Manager knows Jacklet by,
// and the Application event log source it writes lifecycle events to.
const serviceName = "Jacklet"

// serviceDisplayName and serviceDescription are what Services.msc shows.
const (
	serviceDisplayName = "Jacklet Torznab indexer proxy"
	serviceDescription = "Serves torrent sites described by Cardigann definitions over a Torznab API."
)

// defaultStartTimeout is the longest a service start may take before it is
// reported as failed. Opening the database and parsing definitions must both
// finish within this ceiling.
const defaultStartTimeout = 30 * time.Second

// defaultStopTimeout covers the bounded HTTP drain and scraper cleanup, then
// leaves another shutdown interval for closing the database and other final
// process cleanup.
const defaultStopTimeout = 3 * shutdownTimeout

// pendingDeadlineMargin leaves the Service Control Manager time to receive
// the final stopped state after Jacklet gives up waiting for a transition.
const pendingDeadlineMargin = 2 * time.Second

// serviceFailed is the exit code reported when the server could not start
// or stopped with an error, which is what makes a failure visible to
// "sc query" and to the recovery actions configured on the service.
const serviceFailed = 1

// Event log identifiers for the lifecycle events Jacklet reports. The full
// log stream goes to the log file: the Application log is where an
// administrator looks when a service will not start, not a place to put
// one record per request.
const (
	eventStarted = 1
	eventStopped = 2
	eventFailed  = 3
)

// waitHintMillis renders a wait hint for svc.Status, which carries it as
// milliseconds in a uint32.
func waitHintMillis(hint time.Duration) uint32 {
	millis := hint.Milliseconds()
	if millis < 0 {
		return 0
	}
	if millis > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(millis)
}

// serviceHandler runs the server under the Service Control Manager.
type serviceHandler struct {
	cfg          *settings
	events       *eventlog.Log
	logger       *slog.Logger
	runServer    func(context.Context, *slog.Logger, *settings, func()) error
	startTimeout time.Duration
	statuses     chan<- svc.Status
	stopTimeout  time.Duration
}

func newServiceHandler(cfg *settings, events *eventlog.Log, logger *slog.Logger) *serviceHandler {
	return &serviceHandler{
		cfg:          cfg,
		events:       events,
		logger:       logger,
		runServer:    run,
		startTimeout: defaultStartTimeout,
		stopTimeout:  defaultStopTimeout,
	}
}

// Execute runs the server until the Service Control Manager asks it to
// stop, reporting each state transition as it happens.
func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	h.statuses = statuses
	statuses <- svc.Status{
		CheckPoint: 1,
		State:      svc.StartPending,
		WaitHint:   waitHintMillis(h.startTimeout + pendingDeadlineMargin),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- h.runServer(ctx, h.logger, h.cfg, sync.OnceFunc(func() { close(ready) }))
	}()

	// Running is reported only once the listener accepts, so a port
	// already in use surfaces as a service that failed to start rather
	// than one that started and immediately stopped.
	select {
	case <-ready:
	case err := <-serverErr:
		h.report(eventFailed, "Jacklet failed to start: "+errorText(err))
		return true, serviceFailed
	case <-time.After(h.startTimeout):
		h.report(eventFailed, "Jacklet failed to start before the service timeout.")
		return true, serviceFailed
	}

	statuses <- svc.Status{Accepts: accepted, State: svc.Running}
	h.report(eventStarted, "Jacklet started.")

	for {
		select {
		case err := <-serverErr:
			// The server gave up on its own, without being asked to.
			h.report(eventFailed, "Jacklet stopped unexpectedly: "+errorText(err))
			return true, serviceFailed

		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- request.CurrentStatus

			case svc.Stop, svc.Shutdown:
				return h.shutdown(cancel, serverErr)

			default:
				// An unrequested control is ignored: Accepts tells the
				// Service Control Manager which ones Jacklet handles.
			}
		}
	}
}

// shutdown cancels the server and waits for it to finish within the wait
// hint already reported to the Service Control Manager.
func (h *serviceHandler) shutdown(cancel context.CancelFunc, serverErr <-chan error) (bool, uint32) {
	h.statuses <- svc.Status{
		CheckPoint: 1,
		State:      svc.StopPending,
		WaitHint:   waitHintMillis(h.stopTimeout + pendingDeadlineMargin),
	}

	cancel()
	finished, err := waitForServer(serverErr, h.stopTimeout)
	if finished {
		if err != nil {
			h.report(eventFailed, "Jacklet stopped with an error: "+errorText(err))
			return true, serviceFailed
		}
		h.report(eventStopped, "Jacklet stopped.")
		return false, 0
	}
	h.report(eventFailed, "Jacklet did not stop before the service timeout.")
	return true, serviceFailed
}

// waitForServer distinguishes a completed server that returned nil from one
// that did not complete before its service deadline.
func waitForServer(serverErr <-chan error, wait time.Duration) (bool, error) {
	select {
	case err := <-serverErr:
		return true, err
	case <-time.After(wait):
		return false, nil
	}
}

// report writes a lifecycle event to the Application event log, where an
// administrator diagnosing a service that will not start looks first.
//
// A missing or unregistered source is not worth failing over: the same
// detail is in the log file, and refusing to run because an event source
// is absent would be worse than running without one.
func (h *serviceHandler) report(id uint32, message string) {
	if h.events == nil {
		return
	}
	var err error
	switch id {
	case eventFailed:
		err = h.events.Error(id, message)
	default:
		err = h.events.Info(id, message)
	}
	if err != nil {
		h.logger.Warn("could not write to the event log", "error", err)
	}
}

// errorText renders an error for an event log message, including the nil
// case so a caller need not branch.
func errorText(err error) string {
	if err == nil {
		return "the server exited without an error"
	}
	return err.Error()
}

// openEventLog connects to Jacklet's Application event log source, or
// returns nil when it is not registered — which is the ordinary case for
// an interactive run by a user who cannot register one.
func openEventLog() *eventlog.Log {
	events, err := eventlog.Open(serviceName)
	if err != nil {
		return nil
	}
	return events
}
