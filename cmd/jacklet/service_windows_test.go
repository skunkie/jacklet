// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// statusRecorder drains a handler's status channel, keeping the sequence
// of states it reported. A handler blocks on an unread status channel, so
// something has to be reading it for the whole of Execute.
type statusRecorder struct {
	drained chan struct{}
	mu      sync.Mutex
	states  []svc.State
}

type recoveryRecorder struct {
	actions                       []mgr.RecoveryAction
	actionsError                  error
	hasConfiguredNonCrashFailures bool
	isNonCrashFailuresEnabled     bool
	nonCrashFailuresError         error
	resetPeriod                   uint32
}

func (r *recoveryRecorder) SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error {
	r.actions = actions
	r.resetPeriod = resetPeriod
	return r.actionsError
}

func (r *recoveryRecorder) SetRecoveryActionsOnNonCrashFailures(enabled bool) error {
	r.hasConfiguredNonCrashFailures = true
	r.isNonCrashFailuresEnabled = enabled
	return r.nonCrashFailuresError
}

func recordStatuses(statuses <-chan svc.Status) *statusRecorder {
	recorder := &statusRecorder{drained: make(chan struct{})}
	go func() {
		defer close(recorder.drained)
		for status := range statuses {
			recorder.mu.Lock()
			// Repeated checkpoints of one state are progress reports, not
			// transitions, so only a change is recorded.
			if len(recorder.states) == 0 || recorder.states[len(recorder.states)-1] != status.State {
				recorder.states = append(recorder.states, status.State)
			}
			recorder.mu.Unlock()
		}
	}()
	return recorder
}

func (r *statusRecorder) seen() []svc.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]svc.State(nil), r.states...)
}

// awaitDrained waits for the closed status channel to be read to the end.
// A handler returns once it has sent its last status, not once that status
// has been recorded, so asserting on the whole sequence without this races
// the recorder's final append.
func (r *statusRecorder) awaitDrained(t *testing.T) {
	t.Helper()
	select {
	case <-r.drained:
	case <-time.After(defaultStopTimeout):
		require.FailNow(t, "the statuses channel was never closed")
	}
}

// awaitRunning waits for the handler to report itself running, so a test
// that then asks it to stop is not racing the start.
func (r *statusRecorder) awaitRunning(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		return slices.Contains(r.seen(), svc.Running)
	}, defaultStartTimeout, 20*time.Millisecond, "the handler never reported itself running")
}

func TestServiceHandler_RunsUntilStopped(t *testing.T) {
	handler := newServiceHandler(testSettings(t, freePort(t)), nil, slog.New(slog.DiscardHandler))

	statuses := make(chan svc.Status)
	requests := make(chan svc.ChangeRequest)
	recorder := recordStatuses(statuses)

	type executeResult struct {
		code       uint32
		isSpecific bool
	}
	result := make(chan executeResult, 1)
	go func() {
		specific, code := handler.Execute(nil, requests, statuses)
		close(statuses)
		result <- executeResult{code: code, isSpecific: specific}
	}()

	recorder.awaitRunning(t)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}

	select {
	case outcome := <-result:
		require.False(t, outcome.isSpecific, "a requested stop is not a service-specific failure")
		require.Zero(t, outcome.code, "a requested stop is not a failure")
	case <-time.After(defaultStopTimeout):
		require.FailNow(t, "the handler did not stop when asked")
	}

	recorder.awaitDrained(t)
	require.Equal(t, []svc.State{svc.StartPending, svc.Running, svc.StopPending}, recorder.seen(),
		"svc.Run owns the final stopped status and attaches Execute's exit code to it")
}

func TestServiceHandler_AnswersInterrogate(t *testing.T) {
	handler := newServiceHandler(testSettings(t, freePort(t)), nil, slog.New(slog.DiscardHandler))

	statuses := make(chan svc.Status)
	requests := make(chan svc.ChangeRequest)
	recorder := recordStatuses(statuses)

	result := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(nil, requests, statuses)
		close(statuses)
		result <- code
	}()

	recorder.awaitRunning(t)

	// An interrogation echoes the status it was handed, which is how the
	// Service Control Manager polls a running service.
	requests <- svc.ChangeRequest{
		Cmd:           svc.Interrogate,
		CurrentStatus: svc.Status{Accepts: svc.AcceptStop, State: svc.Running},
	}

	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	<-result
	recorder.awaitDrained(t)
	require.Equal(t, []svc.State{svc.StartPending, svc.Running, svc.StopPending}, recorder.seen(),
		"an interrogation reports no new state")
}

// TestServiceHandler_ReportsAStartFailure covers the case native service
// integration exists to make visible: a server that cannot bind must leave
// the service failed, not briefly running and then stopped.
func TestServiceHandler_ReportsAStartFailure(t *testing.T) {
	// Hold the port on the wildcard address, which is the one the server
	// binds: Windows lets a wildcard bind coexist with a loopback-only one,
	// so holding 127.0.0.1 alone would leave the port bindable there.
	//nolint:gosec // G102: the server binds the wildcard address, so holding the port has to as well.
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	handler := newServiceHandler(testSettings(t, port), nil, slog.New(slog.DiscardHandler))

	statuses := make(chan svc.Status)
	requests := make(chan svc.ChangeRequest)
	recorder := recordStatuses(statuses)

	type executeResult struct {
		code       uint32
		isSpecific bool
	}
	result := make(chan executeResult, 1)
	go func() {
		specific, code := handler.Execute(nil, requests, statuses)
		close(statuses)
		result <- executeResult{code: code, isSpecific: specific}
	}()

	// A start that fails returns on its own, at the latest once the
	// handler's own start deadline fires; twice that deadline is a
	// generous margin. Past it the handler has started and is waiting for
	// a control, so asking it to stop turns that into a reported failure
	// rather than a test that hangs until the package timeout.
	var outcome executeResult
	select {
	case outcome = <-result:
	case <-time.After(2 * defaultStartTimeout):
		select {
		case requests <- svc.ChangeRequest{Cmd: svc.Stop}:
		case <-result:
		}
		require.FailNow(t, "the handler did not report a start failure despite the port being in use")
	}

	recorder.awaitDrained(t)
	require.True(t, outcome.isSpecific, "the exit code is a service-specific failure, not a Win32 error")
	require.Equal(t, uint32(serviceFailed), outcome.code, "a failed start reports a non-zero exit code")
	require.NotContains(t, recorder.seen(), svc.Running, "the service never reported itself running")
	require.NotContains(t, recorder.seen(), svc.Stopped,
		"svc.Run must publish stopped once, with the returned failure code")
}

func TestWaitForServer_StopsWaitingAtTheDeadline(t *testing.T) {
	finished, err := waitForServer(make(chan error), 20*time.Millisecond)
	require.NoError(t, err)
	require.False(t, finished, "a hung server must not keep the service pending indefinitely")
}

func TestWaitForServer_DistinguishesANilResultFromATimeout(t *testing.T) {
	serverErr := make(chan error, 1)
	serverErr <- nil

	finished, err := waitForServer(serverErr, time.Second)
	require.NoError(t, err)
	require.True(t, finished, "a clean server exit is a completed shutdown, not a timeout")
}

func TestWaitForServer_PropagatesAServerError(t *testing.T) {
	serverErr := make(chan error, 1)
	want := errors.New("server failed")
	serverErr <- want

	finished, err := waitForServer(serverErr, time.Second)
	require.ErrorIs(t, err, want)
	require.True(t, finished, "a server error is still a completed wait")
}

func TestServiceHandler_ReportsAStartTimeout(t *testing.T) {
	handler := newServiceHandler(nil, nil, slog.New(slog.DiscardHandler))
	handler.runServer = func(ctx context.Context, _ *slog.Logger, _ *settings, _ func()) error {
		<-ctx.Done()
		return nil
	}
	handler.startTimeout = 20 * time.Millisecond

	statuses := make(chan svc.Status, 4)
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), statuses)

	require.True(t, specific)
	require.Equal(t, uint32(serviceFailed), code)
	close(statuses)
	var states []svc.State
	for status := range statuses {
		states = append(states, status.State)
	}
	require.Equal(t, []svc.State{svc.StartPending}, states,
		"a timed-out start never reached running or published stopped")
}

func TestServiceHandler_ReportsAStopTimeout(t *testing.T) {
	handler := newServiceHandler(nil, nil, slog.New(slog.DiscardHandler))
	statuses := make(chan svc.Status, 1)
	handler.statuses = statuses
	handler.stopTimeout = 20 * time.Millisecond

	specific, code := handler.shutdown(func() {}, make(chan error))

	require.True(t, specific)
	require.Equal(t, uint32(serviceFailed), code)
	require.Equal(t, svc.StopPending, (<-statuses).State)
	require.Empty(t, statuses, "a timed-out stop is finalized only by svc.Run")
}

func TestConfigureRecovery_IncludesReturnedServiceErrors(t *testing.T) {
	recorder := &recoveryRecorder{}

	require.NoError(t, configureRecovery(recorder))
	require.Equal(t, []mgr.RecoveryAction{
		{Delay: 5 * time.Second, Type: mgr.ServiceRestart},
		{Delay: 5 * time.Second, Type: mgr.ServiceRestart},
		{Delay: 0, Type: mgr.NoAction},
	}, recorder.actions)
	require.Equal(t, uint32((24 * time.Hour).Seconds()), recorder.resetPeriod)
	require.True(t, recorder.hasConfiguredNonCrashFailures)
	require.True(t, recorder.isNonCrashFailuresEnabled)
}

func TestConfigureRecovery_ReportsEnablingReturnedServiceErrors(t *testing.T) {
	recorder := &recoveryRecorder{nonCrashFailuresError: errors.New("flag refused")}

	err := configureRecovery(recorder)
	require.ErrorContains(t, err, "enabling recovery after a service error")
}

func TestConfigureRecovery_ReportsConfiguringRecoveryActions(t *testing.T) {
	recorder := &recoveryRecorder{actionsError: errors.New("actions refused")}

	err := configureRecovery(recorder)
	require.ErrorContains(t, err, "configuring the service recovery actions")
	require.False(t, recorder.hasConfiguredNonCrashFailures,
		"the failure flag is not configured when the recovery actions failed")
}

// stateSequence is a service that reports each state in turn, settling on
// the last, and refuses a control the way the Service Control Manager
// does: a stop is accepted only from a state that can act on one. Without
// that refusal these tests would pass on a stop sent at any moment.
//
// raceTo, when set, is the state the service has really reached by the
// time a control arrives, whatever the last query reported. That window
// is the one the stop handling cannot close, only survive.
type stateSequence struct {
	controls []svc.Cmd
	current  svc.State
	err      error
	raceTo   svc.State
	refusals int
	states   []svc.State
}

func (s *stateSequence) Query() (svc.Status, error) {
	s.current = s.states[0]
	if len(s.states) > 1 {
		s.states = s.states[1:]
	}
	return svc.Status{State: s.current}, nil
}

func (s *stateSequence) Control(c svc.Cmd) (svc.Status, error) {
	s.controls = append(s.controls, c)
	if s.err != nil {
		return svc.Status{}, s.err
	}
	state := s.current
	if s.raceTo != 0 {
		state = s.raceTo
	}
	switch state {
	case svc.StartPending, svc.StopPending:
		s.refusals++
		return svc.Status{}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	case svc.Stopped:
		s.refusals++
		return svc.Status{}, windows.ERROR_SERVICE_NOT_ACTIVE
	}
	return svc.Status{}, nil
}

// quickenControl shrinks the wait a verb spends on the Service Control
// Manager, so a test reaches the far side of it without sleeping there.
//
// It swaps package-level values, which every test in the package shares,
// so a test that calls this must not also call t.Parallel: the shrunk
// wait would leak into whatever ran beside it, and the failure would
// surface as a timeout somewhere with no apparent connection to here.
func quickenControl(t *testing.T, timeout time.Duration) {
	t.Helper()
	timeoutWas, intervalWas := controlTimeout, controlPollInterval
	t.Cleanup(func() { controlTimeout, controlPollInterval = timeoutWas, intervalWas })
	controlTimeout, controlPollInterval = timeout, time.Millisecond
}

// requireStopped fails unless the service was left stopped without a
// control the Service Control Manager would have refused.
func requireStopped(t *testing.T, service *stateSequence, wantControls []svc.Cmd) {
	t.Helper()
	quickenControl(t, time.Minute)
	require.NoError(t, ensureStopped(service))
	require.Zero(t, service.refusals, "a stop was sent from a state that cannot accept one")
	require.Equal(t, wantControls, service.controls)
}

// A stop already under way is not something to ask for again: the Service
// Control Manager refuses the control on a service that is stopping, and
// for the uninstall that refusal is the difference between removing a
// service that was merely mid-shutdown and giving up on it.
func TestEnsureStopped_WaitsOutAStopAlreadyUnderWay(t *testing.T) {
	requireStopped(t, &stateSequence{
		states: []svc.State{svc.StopPending, svc.StopPending, svc.Stopped},
	}, nil)
}

// The same for a service caught mid-start, which the Service Control
// Manager will not accept a stop for either.
func TestEnsureStopped_WaitsForAStartToFinishBeforeStopping(t *testing.T) {
	requireStopped(t, &stateSequence{
		states: []svc.State{svc.StartPending, svc.Running, svc.Stopped},
	}, []svc.Cmd{svc.Stop})
}

func TestEnsureStopped_AcceptsAServiceThatIsAlreadyStopped(t *testing.T) {
	requireStopped(t, &stateSequence{states: []svc.State{svc.Stopped}}, nil)
}

func TestEnsureStopped_StopsARunningServiceOnce(t *testing.T) {
	requireStopped(t, &stateSequence{
		states: []svc.State{svc.Running, svc.StopPending, svc.Stopped},
	}, []svc.Cmd{svc.Stop})
}

// Asking from a state that accepts a stop is still a guess, because the
// state can move before the control lands. The refusal that follows says
// what the next query would have, so taking it for a failure would be the
// same refusal to act on a service already on its way out.
func TestEnsureStopped_ToleratesAStopThatRacedTheQuery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raceTo svc.State
	}{
		{name: "another stop got there first", raceTo: svc.StopPending},
		{name: "the service exited on its own", raceTo: svc.Stopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quickenControl(t, time.Minute)
			service := &stateSequence{
				raceTo: tc.raceTo,
				states: []svc.State{svc.Running, tc.raceTo, svc.Stopped},
			}

			require.NoError(t, ensureStopped(service))
			require.Equal(t, []svc.Cmd{svc.Stop}, service.controls)
			require.Equal(t, 1, service.refusals, "the refusal this covers never happened")
		})
	}
}

// Every other refusal is still one, and access denied is the one an
// operator meets, so it keeps the hint that says what to do about it.
func TestEnsureStopped_ReportsARefusedStop(t *testing.T) {
	quickenControl(t, time.Minute)
	service := &stateSequence{states: []svc.State{svc.Running}, err: windows.ERROR_ACCESS_DENIED}

	err := ensureStopped(service)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	require.ErrorContains(t, err, "stopping the service")
	require.ErrorContains(t, err, "run this from an elevated command prompt")
}

// A service that accepts the stop and then never acts on it is the case
// the deadline exists for, and the report has to name the state it was
// stuck in rather than the one that was asked for.
func TestEnsureStopped_ReportsAServiceThatNeverStops(t *testing.T) {
	quickenControl(t, 20*time.Millisecond)
	service := &stateSequence{states: []svc.State{svc.Running}}

	err := ensureStopped(service)
	require.ErrorContains(t, err, "the service was still running after")
	require.ErrorContains(t, err, "check the event log")
	require.Equal(t, []svc.Cmd{svc.Stop}, service.controls, "the stop was accepted; it was not acted on")
}

// A start that fails leaves the service stopped, and waiting out the whole
// deadline would report a service that has already finished as slow.
func TestAwaitState_ReportsAServiceThatStoppedAsItStarted(t *testing.T) {
	quickenControl(t, time.Minute)
	service := &stateSequence{states: []svc.State{svc.StartPending, svc.Stopped}}

	err := awaitState(service, svc.Running)
	require.ErrorContains(t, err, "stopped immediately after starting")
}

func TestAwaitState_ReportsAStateItNeverReaches(t *testing.T) {
	quickenControl(t, 20*time.Millisecond)
	service := &stateSequence{states: []svc.State{svc.StartPending}}

	err := awaitState(service, svc.Running)
	require.ErrorContains(t, err, "the service was still starting after")
}
