package community

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

var errHostDeadline = errors.New("community operation timed out")

func (h *Host) drainRemaining() time.Duration {
	if h.drainDeadline.IsZero() {
		grace := h.drainGrace
		if grace <= 0 {
			grace = 35 * time.Second
		}
		h.drainDeadline = time.Now().Add(grace)
	}
	return max(time.Until(h.drainDeadline), 0)
}

// callUntil is sequential, like Call. Cancel is a bounded best-effort write:
// even a child that never reads a pipe-full init cannot block the deadline.
// EOF then keeps the host alive for the same single credential checkpoint
// budget reused by Close, without concurrent scanner and drain readers.
func (h *Host) callUntil(method string, params, out any, deadline time.Time) error {
	return h.callUntilContext(context.Background(), method, params, out, deadline)
}

// Cancellation aborts only this operation and then permits the same bounded
// credential checkpoint used on a timeout. It does not kill a refreshing host
// immediately, nor start a new drain budget when Close is called afterwards.
func (h *Host) callUntilContext(ctx context.Context, method string, params, out any, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if contextDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || contextDeadline.Before(deadline)) {
		deadline = contextDeadline
	}
	if deadline.IsZero() && ctx.Done() == nil {
		return h.Call(method, params, out)
	}
	if !deadline.IsZero() && time.Until(deadline) <= 0 {
		return errHostDeadline
	}
	id := h.next + 1
	done := make(chan error, 1)
	go func() { done <- h.Call(method, params, out) }()
	var timedOut <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timedOut = timer.C
	}
	result := errHostDeadline
	select {
	case err := <-done:
		return err
	case <-timedOut:
	case <-ctx.Done():
		result = ctx.Err()
	}
	// Early caller cancellation must not inherit an operation's unused minute.
	// Cap an existing total deadline, preserving a shorter previously reserved
	// remainder and the shortened grace used by real subprocess fixtures.
	grace := h.drainGrace
	if grace <= 0 {
		grace = 35 * time.Second
	}
	cap := time.Now().Add(grace)
	if h.drainDeadline.IsZero() || cap.Before(h.drainDeadline) {
		h.drainDeadline = cap
	}
	h.drainRemaining() // starts the one shutdown budget before any pipe write
	aborted := make(chan struct{})
	go func() {
		_ = json.NewEncoder(h.input).Encode(map[string]any{"method": "abort", "params": map[string]any{"id": id}})
		close(aborted)
	}()
	abortWait := time.NewTimer(min(100*time.Millisecond, h.drainRemaining()))
	select {
	case <-aborted:
	case <-abortWait.C:
	}
	abortWait.Stop()
	_ = h.input.Close() // also releases a blocked original Call/abort writer
	drain := time.NewTimer(h.drainRemaining())
	defer drain.Stop()
	select {
	case <-done:
	case <-drain.C:
		h.cancel()
		if closer, ok := h.output.(io.Closer); ok {
			_ = closer.Close()
		}
		<-done
	}
	return result
}
