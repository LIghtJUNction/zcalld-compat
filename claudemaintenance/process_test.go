package claudemaintenance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeOwnedProcess() {
	path := os.Args[2]
	if os.Args[1] == "--owned-child" {
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(71)
		}
		time.Sleep(time.Minute)
		return
	}
	self, err := os.Executable()
	if err != nil {
		os.Exit(72)
	}
	child := exec.Command(self, "--owned-child", path)
	if child.Start() != nil {
		os.Exit(73)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	os.Exit(74)
}

func fixturePID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid < 1 {
		t.Fatal("invalid owned fixture pid")
	}
	return pid
}

func TestProcessFinishWaitsForOwnedDescendantAfterParentExit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "owned-child.pid")
	cmd := exec.Command(self, "--owned-parent", path)
	start, _, finish, err := prepareProcess(cmd)
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
	pid := fixturePID(t, path)
	if !ownedFixtureAlive(pid) {
		t.Fatal("fixture descendant exited before cleanup")
	}
	before := time.Now()
	if err = finish(before.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if time.Since(before) > 3100*time.Millisecond || ownedFixtureAlive(pid) {
		t.Fatal("finish returned before owned descendant exited or exceeded its deadline")
	}
}

func TestProcessCancellationDoesNotReportCompletion(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "owned-child.pid")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	begin := time.Now()
	_, started, err := runProcess(ctx, self, []string{"--owned-child", path}, os.Environ(), filepath.Dir(path), nil, false, nil)
	if !started || err == nil {
		t.Fatal("cancelled process was reported as successful", started, err)
	}
	pid := fixturePID(t, path)
	if time.Since(begin) > 5100*time.Millisecond || ownedFixtureAlive(pid) {
		t.Fatal(fmt.Sprintf("owned process %d remained alive after bounded cancellation", pid))
	}
}
