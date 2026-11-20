package community

import (
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
	if deadline.IsZero() {
		return h.Call(method, params, out)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errHostDeadline
	}
	id := h.next + 1
	done := make(chan error, 1)
	go func() { done <- h.Call(method, params, out) }()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
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
	return errHostDeadline
}
