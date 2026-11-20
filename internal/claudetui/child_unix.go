//go:build darwin || linux

package claudetui

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/claudetui/ptyproc"
	"golang.org/x/sys/unix"
)

// child is Claude Code, started in a pseudo-terminal.
type child struct{ cmd *exec.Cmd }

// endSignals are what the helper ends on, its Claude Code with it.
var endSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP}

func (s *shim) start(path string, args []string) error {
	m, name, err := openPTY()
	if err != nil {
		return err
	}
	tty, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return err
	}
	// wide, so a line of Claude Code's screen is a line
	_ = unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: screenRows, Col: screenCols})
	cmd := proc.Command(path, args...)
	cmd.Env = childEnv(s.env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// its own session, the terminal its controlling one: the shim ending
	// closes the terminal, and Claude Code is hung up on with it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		m.Close()
		tty.Close()
		return err
	}
	tty.Close()
	s.pty, s.cmd = m, cmd
	s.waited = make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(s.waited)
		s.ended(err, false)
	}()
	go s.read()
	return nil
}

// stopChild ends the session's owned process group. The adapter's own process
// group cannot reach this separate PTY session. Descendants that deliberately
// create another session are outside this process-group contract.
func (s *shim) stopChild() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	pid := s.cmd.Process.Pid
	until := time.Now().Add(stopGrace + stopReap)
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if s.waitGroup(pid, until.Add(-stopReap)) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if s.waitGroup(pid, until) {
		return nil
	}
	return errDrain
}

func (s *shim) waitGroup(pid int, until time.Time) bool {
	for {
		if syscall.Kill(-pid, 0) == syscall.ESRCH && waitReaped(s.waited, time.Now()) {
			return true
		}
		remaining := time.Until(until)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(10*time.Millisecond, remaining))
	}
}

// imagePadOS: a terminal here passes the bracketed-paste marks (imagePad).
const imagePadOS = 0

// cutHint is what a message Claude Code didn't get whole (whole.go) is
// told of where it was lost: nothing known here.
func cutHint() string { return "" }
