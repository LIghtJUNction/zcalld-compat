package claudetui

import (
	"errors"
	"io"
	"time"
)

// A caller must reserve cleanup within its original deadline. A stop spends
// at most three seconds asking the child to exit and 500 ms confirming forced
// termination; it never starts another operation budget. The maintenance
// adapter ends helper work eight seconds before its overall deadline.
const stopGrace = 3 * time.Second
const stopReap = 500 * time.Millisecond

var errDrain = errors.New("owned Claude process termination could not be confirmed")

// EOF, a signal and the helper deadline may race. They all observe the same
// finalizer and its result. A normal EOF cannot report success after merely
// submitting SIGKILL or TerminateProcess.
func (s *shim) stop() error {
	s.stopOnce.Do(func() {
		s.stopErr = s.stopChild()
		if terminal, ok := s.pty.(io.Closer); ok {
			_ = terminal.Close()
		}
	})
	return s.stopErr
}

func waitReaped(done <-chan struct{}, until time.Time) bool {
	if done == nil {
		return true
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
