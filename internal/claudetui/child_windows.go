package claudetui

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/LIghtJUNction/zcalld-compat/internal/claudetui/ptyproc"
)

// child is Claude Code, started in a pseudo console (ConPTY): Windows' own
// terminal for a program that draws, as Windows Terminal runs one in.
type child struct {
	proc      windows.Handle
	console   windows.Handle
	closeOnce sync.Once
}

// endSignals are what the helper ends on, its Claude Code with it: Ctrl+C,
// and its console closed. The gateway ends a run by killing its job, which
// no handler hears: the images folder is then left for another helper to
// remove (dropLeft).
var endSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// pseudoConsole holds system CreatePseudoConsole and ClosePseudoConsole.
// The pinned upstream notes that Windows 10 conhost 10.0.19041 can lose
// bracketed-paste markers and 1024-byte blocks. This optional standalone
// component reports an incomplete paste instead of falling back to print.
type pseudoConsole struct {
	create, close *windows.LazyProc
	// system: Windows' own, kernel32's
	system bool
}

// Standalone builds use system ConPTY; no side-loaded or downloaded DLL.
var console = sync.OnceValue(func() pseudoConsole {
	k := windows.NewLazySystemDLL("kernel32.dll")
	return pseudoConsole{k.NewProc("CreatePseudoConsole"), k.NewProc("ClosePseudoConsole"), true}
})

// win11 is Windows 11's first build: the ConPTY of every one since keeps
// a paste's marks and every block of it.
const win11 = 22000

// cutHint is what a message Claude Code didn't get whole (whole.go) is
// told of where it was lost: on Windows 10's own ConPTY, how to use
// another (#5205).
func cutHint() string {
	if !console().system || windows.RtlGetVersion().BuildNumber >= win11 {
		return ""
	}
	// short, for the bridge's log line to hold it whole
	return "; this Windows 10 system ConPTY may truncate long pastes; a supported console that preserves bracketed paste is required"
}

func createPseudoConsole(size windows.Coord, in, out windows.Handle, h *windows.Handle) error {
	r, _, _ := console().create.Call(uintptr(*(*uint32)(unsafe.Pointer(&size))), uintptr(in), uintptr(out), 0, uintptr(unsafe.Pointer(h)))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func closePseudoConsole(h windows.Handle) { _, _, _ = console().close.Call(uintptr(h)) }

func (s *shim) start(path string, args []string) error {
	// Windows 10 before 1809 has none, and x/sys panics calling it there
	if err := console().create.Find(); err != nil {
		return fmt.Errorf("no pseudo console on this Windows: %w", err)
	}
	// a .cmd (npm's shim) is run by cmd.exe, its arguments quoted as
	// Claude Code is run with elsewhere (proc)
	cmd := proc.Command(path, args...)
	if cmd.Err != nil {
		return cmd.Err
	}
	line := windows.ComposeCommandLine(cmd.Args)
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		line = cmd.SysProcAttr.CmdLine
	}
	// what is typed goes in at inW, what Claude Code draws comes out at
	// outR; the console holds the other ends
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return err
	}
	var pc windows.Handle
	// wide, so a line of Claude Code's screen is a line
	err := createPseudoConsole(windows.Coord{X: screenCols, Y: screenRows}, inR, outW, &pc)
	// the console has its own of these
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	in, out := os.NewFile(uintptr(inW), "conpty-in"), os.NewFile(uintptr(outR), "conpty-out")
	if err != nil {
		in.Close()
		out.Close()
		return fmt.Errorf("a pseudo console: %w", err)
	}
	h, err := startIn(pc, cmd.Path, line, childEnv(s.env...))
	if err != nil {
		closePseudoConsole(pc)
		in.Close()
		out.Close()
		return err
	}
	s.pty = conpty{in: in, out: out}
	s.proc, s.console = h, pc
	s.waited = make(chan struct{})
	go func() {
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		var code uint32
		var exit error
		if err := windows.GetExitCodeProcess(h, &code); err != nil {
			exit = err
		} else if code != 0 {
			exit = fmt.Errorf("exit status %d", code)
		}
		close(s.waited)
		s.ended(exit, true)
	}()
	go s.read()
	return nil
}

// startIn starts the program at app, with the command line line and the
// environment env, attached to console.
func startIn(console windows.Handle, app, line string, env []string) (windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	// the attribute's value is the console's handle itself
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		return 0, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// standard handles of none: without this, the helper's own (the
	// bridge's pipes) are Claude Code's, and it draws into them rather
	// than the console (microsoft/terminal#11276)
	si.Flags = windows.STARTF_USESTDHANDLES
	appW, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return 0, err
	}
	lineW, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return 0, err
	}
	block, err := envBlock(env)
	if err != nil {
		return 0, err
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(appW, lineW, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, block, nil, &si.StartupInfo, &pi); err != nil {
		return 0, err
	}
	windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

// envBlock is env as CreateProcess takes it: each NUL-ended, then a NUL.
func envBlock(env []string) (*uint16, error) {
	var b []uint16
	for _, e := range env {
		u, err := windows.UTF16FromString(e)
		if err != nil {
			return nil, err
		}
		b = append(b, u...)
	}
	b = append(b, 0)
	return &b[0], nil
}

// conpty is a pseudo console's two ends as one terminal.
type conpty struct{ in, out *os.File }

func (c conpty) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c conpty) Write(p []byte) (int, error) { return c.in.Write(p) }
func (c conpty) Close() error {
	_ = c.in.Close()
	return c.out.Close()
}

// closeConsole closes Claude Code's console, which tells it so, as closing
// a terminal window does (CTRL_CLOSE_EVENT). It returns at once: before
// Windows 11 24H2, ClosePseudoConsole waits for its output to be read.
func (s *shim) closeConsole() {
	s.closeOnce.Do(func() {
		go closePseudoConsole(s.console)
	})
}

// stopChild confirms the direct ConPTY process has exited. The invoking
// adapter additionally owns and drains its Windows Job, including descendants.
func (s *shim) stopChild() error {
	if s.proc == 0 {
		return nil
	}
	until := time.Now().Add(stopGrace + stopReap)
	s.closeConsole()
	if !waitReaped(s.waited, until.Add(-stopReap)) {
		_ = windows.TerminateProcess(s.proc, 1)
	}
	if !waitReaped(s.waited, until) {
		return errDrain
	}
	// The waiter has already read the exit status before closing waited.
	_ = windows.CloseHandle(s.proc)
	return nil
}

// imagePadOS: a pseudo console may drop the bracketed-paste marks, so an
// image's path is sent as a read long enough to be a paste, over 800
// characters and under the 1024 bytes Windows 10's conhost can lose
// (imagePad).
const imagePadOS = 900
