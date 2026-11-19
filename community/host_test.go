//go:build !windows

package community

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCLIHostCloseDrainsBeforeCredentialCheckpoint(t *testing.T) {
	dir := t.TempDir()
	checkpoint := filepath.Join(dir, "checkpoint")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; sleep 0.1; head -c 2097152 /dev/zero; printf rotated > \"$1\"", "fixture", checkpoint)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &Host{cmd: cmd, cancel: cancel, input: in, output: out, scan: bufio.NewScanner(out)}
	h.Close()
	b, err := os.ReadFile(checkpoint)
	if err != nil || string(b) != "rotated" {
		t.Fatal("host was killed before its private write completed")
	}
	h.Close()
}
