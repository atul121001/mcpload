package client

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Cancellation (MCP "Cancellation" utility).
//
// A call made with CallOptions.CancelAfter stops waiting when no response
// arrived within that time and cancels the request on the wire:
//
//   - stateful protocols (2025-xx): a notifications/cancelled
//     {requestId, reason} is POSTed with the session headers. The response
//     stream of the call is then kept open for Options.CancelWait, and a
//     response for the cancelled id that still arrives is ignored (the spec:
//     the sender SHOULD ignore it) but counted as a late response. The
//     stream is closed afterwards.
//   - stateless (2026-07-28 streamable HTTP): closing the response stream is
//     the cancellation signal and no notification is expected, so the stream
//     is closed at once and late responses cannot be observed.
//
// A request that times out (Options.Timeout) is cancelled the same way, as
// both revisions say a sender SHOULD: stateful requests (except initialize)
// get a notifications/cancelled in the background; in stateless mode the
// timeout already closed the stream.

// Cancel reasons (CancelStats.Reason, the `reason` tag).
const (
	CancelReasonClient  = "client"  // CallOptions.CancelAfter
	CancelReasonTimeout = "timeout" // Options.Timeout
)

// Cancel outcomes (CancelStats.Outcome, the `outcome` tag).
const (
	// CancelOutcomeCancelled: the cancellation was sent (or the stream
	// closed) and no response for the request arrived afterwards.
	CancelOutcomeCancelled = "cancelled"
	// CancelOutcomeLateResponse: a response for the cancelled request still
	// arrived within CancelWait after the notification (it was ignored).
	CancelOutcomeLateResponse = "late_response"
	// CancelOutcomeCompleted: the call finished before CancelAfter; nothing
	// was cancelled.
	CancelOutcomeCompleted = "completed"
	// CancelOutcomeSendFailed: the notifications/cancelled POST failed
	// (ErrorType says why); the stream was closed instead.
	CancelOutcomeSendFailed = "send_failed"
)

// DefaultCancelWait is how long the response stream of a cancelled stateful
// call is still read for a late response when Options.CancelWait is 0.
const DefaultCancelWait = 2 * time.Second

// CancelStats describes one cancellation (or a CancelAfter call that
// completed first).
type CancelStats struct {
	Method    string // method of the cancelled request, e.g. "tools/call"
	Tool      string
	Protocol  string
	Reason    string // CancelReason*
	Outcome   string // CancelOutcome*
	ErrorType string // send_failed: the notification's error type
	Status    int    // HTTP status of the notification POST; 0 when none was sent
	Start     time.Time
	// Duration is the time to send the cancellation: from the decision to
	// cancel until the notification POST completed (stateful) or the stream
	// was closed (stateless). 0 for completed.
	Duration time.Duration
	// LateAfter is the time from the decision to cancel until the late
	// response arrived (late_response only).
	LateAfter time.Duration
}

// CancelObserver is optionally implemented by an Observer to receive
// cancellation events. A stateful client cancellation is reported after
// CancelWait (once its outcome is known), after the call has returned.
type CancelObserver interface {
	OnCancel(CancelStats)
}

func (s *Session) cancelWait() time.Duration {
	if s.opts.CancelWait > 0 {
		return s.opts.CancelWait
	}
	return DefaultCancelWait
}

func (s *Session) reportCancel(ctx context.Context, cs CancelStats) {
	if o, ok := s.obs(ctx).(CancelObserver); ok {
		o.OnCancel(cs)
	}
}

// sendCancelled POSTs notifications/cancelled for the request ex (stateful).
func (s *Session) sendCancelled(ctx context.Context, ex exchange, reason string) exchangeResult {
	return s.post(ctx, exchange{
		method: "notifications/cancelled", sessionID: ex.sessionID, protoHdr: ex.protoHdr,
		params: map[string]any{"requestId": *ex.id, "reason": reason},
	})
}

// cancelInFlight cancels the call ex whose round trip is still running in a
// goroutine (it delivers on done; cancel closes its stream). The call has
// already been reported as cancelled; this returns once the cancellation is
// sent, and the cancel event follows when its outcome is known.
func (s *Session) cancelInFlight(ctx context.Context, cancel context.CancelFunc, ex exchange, done <-chan wire, answers *sync.WaitGroup) {
	cs := CancelStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex), Reason: CancelReasonClient, Start: time.Now()}
	if ex.stateless {
		// 2026-07-28 streamable HTTP: closing the response stream is the
		// cancellation; no notification is sent.
		cancel()
		<-done
		cs.Duration, cs.Outcome = time.Since(cs.Start), CancelOutcomeCancelled
		s.reportCancel(ctx, cs)
		return
	}
	n := s.sendCancelled(ctx, ex, fmt.Sprintf("mcpload: cancelled after %s", ex.cancelAfter))
	cs.Duration, cs.Status = time.Since(cs.Start), n.stats.Status
	if n.err != nil {
		// Closing the stream is the only signal left.
		cancel()
		cs.Outcome, cs.ErrorType = CancelOutcomeSendFailed, n.err.Type
		go func() {
			<-done
			answers.Wait()
			s.reportCancel(ctx, cs)
		}()
		return
	}
	go func() {
		t := time.NewTimer(s.cancelWait())
		defer t.Stop()
		cs.Outcome = CancelOutcomeCancelled
		select {
		case w := <-done:
			// The stream ended: either the late response, or the server
			// closed it without one.
			if w.msg != nil {
				cs.Outcome, cs.LateAfter = CancelOutcomeLateResponse, time.Since(cs.Start)
			}
			cancel()
		case <-t.C:
			cancel()
			<-done
		}
		answers.Wait()
		s.reportCancel(ctx, cs)
	}()
}

// cancelOnTimeout cancels a request that timed out (err is its outcome).
// Stateful: notifications/cancelled is sent in the background (initialize
// MUST NOT be cancelled); stateless: the timeout already closed the stream.
func (s *Session) cancelOnTimeout(ctx context.Context, ex exchange, err *Error) {
	if err == nil || err.Type != ErrTimeout || ex.id == nil || ex.method == "initialize" || ex.method == "server/discover" {
		return
	}
	cs := CancelStats{Method: ex.method, Tool: ex.tool, Protocol: s.protocolTag(ex), Reason: CancelReasonTimeout,
		Outcome: CancelOutcomeCancelled, Start: time.Now()}
	if ex.stateless {
		s.reportCancel(ctx, cs)
		return
	}
	if ctx.Err() != nil {
		return // the caller is gone: the notification could not be sent
	}
	go func() {
		n := s.sendCancelled(ctx, ex, "mcpload: request timed out")
		cs.Duration, cs.Status = time.Since(cs.Start), n.stats.Status
		if n.err != nil {
			cs.Outcome, cs.ErrorType = CancelOutcomeSendFailed, n.err.Type
		}
		s.reportCancel(ctx, cs)
	}()
}
