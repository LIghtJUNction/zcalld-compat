//go:build windows

package claudemaintenance

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func ownedFixtureAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	return err != nil || state != windows.WAIT_OBJECT_0
}

func TestProcessStopThenFinishWaitsForOwnedDescendant(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "owned-child.pid")
	cmd := exec.Command(self, "--owned-parent", path)
	start, stop, finish, err := prepareProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = finish(time.Now().Add(3 * time.Second)) }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err = start(ctx); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Keep the same handle across stop and finish: a new PID lookup cannot
	// accidentally turn PID reuse into an apparent successful cleanup.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(fixturePID(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if state, err := windows.WaitForSingleObject(handle, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("fixture descendant was not alive before stop", state, err)
	}
	before := time.Now()
	deadline := before.Add(3 * time.Second)
	if err = stop(); err != nil {
		t.Fatal(err)
	}
	if err = finish(deadline); err != nil {
		t.Fatal(err)
	}
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatal("finish did not wait for the captured descendant handle", state, err)
	}
	if time.Since(before) > 3100*time.Millisecond {
		t.Fatal("stop and finish exceeded their shared deadline")
	}
}

func TestProcessWaitRequiresExitAcknowledgement(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--owned-child", filepath.Join(t.TempDir(), "owned-child.pid"))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	expired := time.Now().Add(-time.Second)
	if err = waitOwnedProcess(handle, expired); !errors.Is(err, errProcessDrain) {
		t.Fatal("an unsignaled process handle was accepted at the deadline", err)
	}
	if state, err := windows.WaitForSingleObject(handle, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("the wait helper changed or lost the live process", state, err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// Expiry must not hide an already completed exit acknowledgement either.
	if err = waitOwnedProcess(handle, expired); err != nil {
		t.Fatal("an exited process handle was not acknowledged", err)
	}
}
