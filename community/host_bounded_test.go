package community

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestQuotaNoInputChild(t *testing.T) {
	if os.Getenv("ZCALLD_TEST_NO_INPUT") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}
func TestQuotaDeadlineWhenChildNeverReadsPipeFullInit(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestQuotaNoInputChild$")
	cmd.Env = append(os.Environ(), "ZCALLD_TEST_NO_INPUT=1")
	in, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	h := &Host{cmd: cmd, cancel: cancel, input: in, output: out, scan: bufio.NewScanner(out), drainGrace: 200 * time.Millisecond}
	started := time.Now()
	var value any
	e = h.callUntil("init", map[string]any{"options": strings.Repeat("x", 2<<20)}, &value, time.Now().Add(30*time.Millisecond))
	if !errors.Is(e, errHostDeadline) {
		t.Fatal(e)
	}
	deadline := h.drainDeadline
	h.Close()
	if h.drainDeadline != deadline {
		t.Fatal("Close restarted the shutdown budget")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("pipe blocked request/shutdown deadline")
	}
	if cmd.ProcessState == nil {
		t.Fatal("owned unresponsive child was not reaped")
	}
}

func TestMaintenanceContextCancelsPipeFullInitWithinSingleDrain(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	processContext, kill := context.WithCancel(context.Background())
	defer kill()
	cmd := exec.CommandContext(processContext, executable, "-test.run=^TestQuotaNoInputChild$")
	cmd.Env = append(os.Environ(), "ZCALLD_TEST_NO_INPUT=1")
	in, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	h := &Host{cmd: cmd, cancel: kill, input: in, output: out, scan: bufio.NewScanner(out), drainGrace: 200 * time.Millisecond, drainDeadline: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	e = h.callUntilContext(ctx, "init", map[string]any{"options": strings.Repeat("x", 2<<20)}, nil, time.Now().Add(25*time.Second))
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	deadline := h.drainDeadline
	h.Close()
	if deadline != h.drainDeadline || time.Since(start) > 2*time.Second || cmd.ProcessState == nil {
		t.Fatal("cancellation ignored or drain restarted")
	}
}

func TestMaintenanceAlreadyCancelledStartsNoHost(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := t.TempDir() + "/absent"
	_, err := startBuiltinCodexHostContext(ctx, root, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("cancelled init created state")
	}
}
