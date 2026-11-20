package desktop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

func configFixture(t *testing.T, fields map[string]any) (string, string) {
	t.Helper()
	parent := inbox(t)
	root := filepath.Join(parent, "extensions")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(root, 0700); err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(parent, "desktop", "inbox")
	if err := os.MkdirAll(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if fields != nil {
		b, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		writePrivate(t, filepath.Join(parent, "desktop.json"), b)
	}
	return root, queue
}

func TestReadConfigIsExplicitAndReadOnly(t *testing.T) {
	root, queue := configFixture(t, nil)
	c, e := ReadConfig(root)
	if e != nil || c.Enabled || c.QuotaEnabled() {
		t.Fatal(c, e)
	}
	path := filepath.Join(filepath.Dir(root), "desktop.json")
	if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("missing settings were created")
	}
	for _, fields := range []map[string]any{
		{"usage_alert": 85}, {"enabled": false, "usage_alert": 85},
		{"enabled": true, "mask_alert": true},
	} {
		b, _ := json.Marshal(fields)
		writePrivate(t, path, b)
		before, _ := os.ReadFile(path)
		c, e = ReadConfig(root)
		if e != nil || c.QuotaEnabled() {
			t.Fatal(c, e)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("read mutated settings")
		}
	}
	b, _ := json.Marshal(map[string]any{"enabled": true, "usage_alert": 85, "language": "zh-CN"})
	writePrivate(t, path, b)
	c, e = ReadConfig(root)
	if e != nil || !c.QuotaEnabled() || c.Inbox != queue || c.Language != "zh-CN" || c.Backend != "native" {
		t.Fatal(c, e)
	}
	if len(eventFiles(t, queue)) != 0 {
		t.Fatal("read published an event")
	}
}

func TestReadConfigRejectsInvalidSchemaAndBounds(t *testing.T) {
	root, _ := configFixture(t, nil)
	path := filepath.Join(filepath.Dir(root), "desktop.json")
	for _, body := range []string{
		`{"enabled":true,"enabled":false}`,
		`{"enabled":null}`, `{"enabled":"yes"}`, `{"usage_alert":100.1}`,
		`{"usage_alert":101}`, `{"balance_alert":-1}`, `{"balance_alert":1e999}`,
		`{"reset_reminder_hours":8785}`, `{"turn_done_seconds":86401}`,
		`{"language":"xx"}`, `{"backend":"shell"}`, `{"unknown":"PRIVATE"}`,
		`{"inbox":null}`, `{"inbox":""}`, `{"state_dir":"../other"}`,
		`{"notification_helper":""}`, `{"notification_helper":"relative"}`,
		`{}{}`, `[]`, strings.Repeat(" ", MaxConfigBytes+1),
	} {
		writePrivate(t, path, []byte(body))
		if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
			t.Fatalf("accepted invalid settings: %q (%v)", body[:min(len(body), 100)], e)
		}
	}
	for _, field := range []string{"usage_alert", "balance_alert", "reset_reminder_hours"} {
		b, _ := json.Marshal(map[string]any{"enabled": true, field: 1})
		writePrivate(t, path, b)
		c, e := ReadConfig(root)
		if e != nil || !c.QuotaEnabled() {
			t.Fatal(field, c, e)
		}
	}
}

func TestReadConfigRejectsUnsafeNodesWithoutRepair(t *testing.T) {
	root, queue := configFixture(t, map[string]any{"enabled": true, "usage_alert": 80})
	parent := filepath.Dir(root)
	path := filepath.Join(parent, "desktop.json")
	if err := os.RemoveAll(queue); err != nil {
		t.Fatal(err)
	}
	if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
		t.Fatal("missing inbox accepted", e)
	}
	if _, e := os.Stat(queue); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("inbox created")
	}
	if err := os.WriteFile(queue, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
		t.Fatal("file inbox accepted", e)
	}
	if err := os.Remove(queue); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
		t.Fatal("public config accepted", e)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0644 {
		t.Fatal("read repaired permissions")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, path); err != nil {
		t.Fatal(err)
	}
	if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
		t.Fatal("symlink config accepted", e)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	moved := queue + ".saved"
	if err := os.Rename(queue, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, queue); err != nil {
		t.Fatal(err)
	}
	if _, e := ReadConfig(root); !errors.Is(e, ErrConfig) {
		t.Fatal("symlink inbox accepted", e)
	}
}
