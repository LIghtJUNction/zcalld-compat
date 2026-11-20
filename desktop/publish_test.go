package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

func inbox(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func eventFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
func writePrivate(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledAndMissingDoNotCreateState(t *testing.T) {
	if err := Publish("", 0, make(chan int)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox(t), "disabled")
	if err := Publish(path, 2, map[string]int{"version": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publisher created inbox")
	}
}
func TestAtomicEnvelopePermissionsAndClaims(t *testing.T) {
	dir := inbox(t)
	event := map[string]any{"version": 1, "observation_id": "synthetic-1", "counts": map[string]int{"email": 2}}
	if err := Publish(dir, 2, event); err != nil {
		t.Fatal(err)
	}
	files := eventFiles(t, dir)
	if len(files) != 1 {
		t.Fatal(files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Kind  uint32         `json:"kind"`
		Event map[string]any `json:"event"`
	}
	if json.Unmarshal(b, &decoded) != nil || decoded.Kind != 2 || decoded.Event["observation_id"] != "synthetic-1" {
		t.Fatal(string(b))
	}
	if !privatefile.Check(files[0]) {
		t.Fatal("event is not private")
	}
	if err := os.Rename(files[0], filepath.Join(dir, ".claim-42-1.json")); err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, 1, map[string]int{"version": 1}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pending-") {
			t.Fatal("pending leak")
		}
	}
}
func TestBoundsCountClaimedAndPendingFiles(t *testing.T) {
	dir := inbox(t)
	for i := 0; i < MaxEvents; i++ {
		writePrivate(t, filepath.Join(dir, fmt.Sprintf(".claim-1-%d.json", i)), []byte("{}"))
	}
	if err := Publish(dir, 1, map[string]int{"version": 1}); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	if len(eventFiles(t, dir)) != MaxEvents {
		t.Fatal("full inbox changed")
	}
	other := inbox(t)
	writePrivate(t, filepath.Join(other, ".pending-abandoned"), make([]byte, MaxInboxBytes))
	if err := Publish(other, 2, map[string]int{"version": 1}); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
}
func TestInvalidKindPayloadAndSymlinks(t *testing.T) {
	dir := inbox(t)
	for _, tc := range []struct {
		kind  uint32
		event any
	}{{0, map[string]int{}}, {3, map[string]int{}}, {1, nil}, {1, []int{1}}, {1, map[string]string{"large": strings.Repeat("x", MaxEventBytes)}}} {
		if err := Publish(dir, tc.kind, tc.event); !errors.Is(err, ErrInvalid) {
			t.Fatalf("kind %d: %v", tc.kind, err)
		}
	}
	if len(eventFiles(t, dir)) != 0 {
		t.Fatal("invalid event published")
	}
	if runtime.GOOS == "windows" {
		return
	}
	link := filepath.Join(inbox(t), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := Publish(link, 1, map[string]int{}); !errors.Is(err, ErrUnsafe) {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := Publish(dir, 1, map[string]int{}); !errors.Is(err, ErrUnsafe) {
		t.Fatal(err)
	}
}
func TestConcurrentWritersNeverExceedCapacity(t *testing.T) {
	dir := inbox(t)
	for i := 0; i < MaxEvents-1; i++ {
		writePrivate(t, filepath.Join(dir, fmt.Sprintf(".claim-9-%d.json", i)), []byte("{}"))
	}
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			err := Publish(dir, 2, map[string]int{"version": 1})
			if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrFull) {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if got := len(eventFiles(t, dir)); got != MaxEvents {
		t.Fatalf("events=%d", got)
	}
}
func TestCrossProcessPublisherLock(t *testing.T) {
	if dir := os.Getenv("ZCALLD_DESKTOP_LOCK_FIXTURE"); dir != "" {
		if err := Publish(dir, 2, map[string]int{"version": 1}); !errors.Is(err, ErrBusy) {
			t.Fatalf("want busy: %v", err)
		}
		return
	}
	dir := inbox(t)
	f, err := openLock(filepath.Join(dir, ".publish.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := privatefile.Protect(f.Name(), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := tryLock(f)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessPublisherLock$")
	cmd.Env = append(os.Environ(), "ZCALLD_DESKTOP_LOCK_FIXTURE="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v: %s", err, output)
	}
	if len(eventFiles(t, dir)) != 0 {
		t.Fatal("locked publisher wrote")
	}
}
