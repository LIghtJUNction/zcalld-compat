package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/buildinfo"
)

func TestVersionIsDeclarative(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "not-an-account")
	t.Setenv("MAGPIE_CLAUDE_TUI_WSL", "invalid")
	var b bytes.Buffer
	if err := run([]string{"--version"}, &b); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(b.String()) != buildinfo.String("zcalld-claude-tui") {
		t.Fatalf("unexpected version: %q", b.String())
	}
}

func TestCapabilitiesAreDeclarative(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "not-an-account")
	t.Setenv("MAGPIE_CLAUDE_TUI_WSL", "invalid")
	var b bytes.Buffer
	if err := run([]string{"--capabilities"}, &b); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["schema_version"] != float64(1) || v["interactive"] != true || v["print_fallback"] != false || v["wsl"] != false || v["source_revision"] != sourceRevision {
		t.Fatalf("unexpected capability declaration: %v", v)
	}
}

func TestNativeProfileLayoutsRemainDistinct(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".claude")
	base := []string{"--claude-binary", filepath.Join(home, "claude"), "--config-dir", config,
		"--work-dir", filepath.Join(home, "work"), "--deadline-ms", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10)}
	for _, tc := range []struct {
		profile, nativeHome string
		ok                  bool
	}{
		{filepath.Join(home, ".claude.json"), home, true},
		{filepath.Join(config, ".claude.json"), "", true},
		{filepath.Join(home, "different.json"), "", false},
	} {
		l, err := parse(append(append([]string{}, base...), "--profile", tc.profile))
		if (err == nil) != tc.ok || err == nil && l.nativeHome != tc.nativeHome {
			t.Fatalf("layout %q: nativeHome %q, err %v", tc.profile, l.nativeHome, err)
		}
	}
}

func TestMaintenanceCannotSelectExistingSession(t *testing.T) {
	home := t.TempDir()
	base := []string{"--claude-binary", filepath.Join(home, "claude"), "--config-dir", filepath.Join(home, ".claude"),
		"--profile", filepath.Join(home, ".claude.json"), "--work-dir", home, "--deadline-ms", "1", "--"}
	for _, a := range []string{"--resume", "--resume=existing", "--continue", "-c", "--session-id=existing"} {
		if _, err := parse(append(append([]string{}, base...), a)); err == nil {
			t.Fatalf("accepted %q", a)
		}
	}
}
