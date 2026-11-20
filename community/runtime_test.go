package community

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

func runtimeFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(root, 0700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "bun"
	if runtime.GOOS == "windows" {
		name += ".exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("fixture; never executed"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return root
}

func runtimeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRuntimeRefreshesOnlyOwnedBuiltinPathsAndPreservesState(t *testing.T) {
	root := runtimeFixture(t)
	old, sums := legacyBuiltinFixture(t, root)
	entries := []map[string]any{
		{"name": "builtin-codex", "kind": "community", "source": "builtin-codex",
			"path": filepath.Join(old, "codex.mjs"), "sha256": sums["codex.mjs"], "enabled": false,
			"version": "kept-version", "options": map[string]any{"keep": true}, "future": json.Number("9007199254740993")},
		{"name": "builtin-gemini", "kind": "community", "source": "builtin-gemini",
			"path": filepath.Join(old, "codeassist.mjs"), "sha256": sums["codeassist.mjs"], "enabled": true},
		{"name": "community", "kind": "community", "source": "local",
			"path": filepath.Join(root, "community.mjs"), "enabled": true, "unknown": []any{"kept"}},
	}
	registry := map[string]any{
		"version": 1, "plugins": entries, "accounts": map[string]any{"fixture-account": map[string]any{"enabled": false, "unknown": true}},
		"account_defaults":  map[string]any{"builtin:codex": "fixture-account"},
		"account_policies":  map[string]any{"fixture-account": map[string]any{"codex_no_credits": true}},
		"mcp_tools_enabled": true, "future": map[string]any{"large": json.Number("9007199254740993")},
	}
	before, _ := json.Marshal(registry)
	if err := Atomic(filepath.Join(root, "registry.json"), before, 0600); err != nil {
		t.Fatal(err)
	}
	auth := []byte("{\"fixture\":\"never read or rewritten\"}\n")
	if err := Atomic(filepath.Join(root, "plugin-auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureRuntime(root); err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(filepath.Join(root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	after := runtimeJSON(t, afterBytes)
	rows := after["plugins"].([]any)
	for index, file := range []string{"codex.mjs", "codeassist.mjs"} {
		row := rows[index].(map[string]any)
		path := row["path"].(string)
		if path == entries[index]["path"] || filepath.Base(path) != file {
			t.Fatalf("builtin target did not refresh: %s", file)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		want, err := fileHash(path)
		if err != nil || row["sha256"] != want {
			t.Fatal("builtin checksum does not match the current embedded module")
		}
		row["path"], row["sha256"] = entries[index]["path"], entries[index]["sha256"]
	}
	if !reflect.DeepEqual(after, runtimeJSON(t, before)) {
		t.Fatal("runtime refresh changed unrelated registry state")
	}
	gotAuth, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	if !bytes.Equal(gotAuth, auth) {
		t.Fatal("runtime refresh changed account credentials")
	}
	if err := ConfigureRuntime(root); err != nil {
		t.Fatal(err)
	}
	twice, _ := os.ReadFile(filepath.Join(root, "registry.json"))
	if !bytes.Equal(twice, afterBytes) {
		t.Fatal("runtime refresh is not idempotent")
	}
}

func TestRuntimeDoesNotAdoptOtherPluginsOrAddBuiltinEntries(t *testing.T) {
	root := runtimeFixture(t)
	old := filepath.Join(root, "runtime", "builtin", strings.Repeat("a", 24))
	registry := map[string]any{"version": 1, "plugins": []map[string]any{
		{"name": "builtin-codex", "source": "git:custom", "kind": "community", "path": filepath.Join(old, "codex.mjs")},
		{"name": "builtin-copilot", "source": "builtin-copilot", "kind": "community", "path": filepath.Join(root, "custom-copilot.mjs")},
		{"name": "custom-name", "source": "builtin-gemini", "kind": "community", "path": filepath.Join(old, "codeassist.mjs")},
		{"name": "builtin-antigravity", "source": "builtin-antigravity", "kind": "native", "path": filepath.Join(old, "antigravity.mjs")},
	}}
	before, _ := json.Marshal(registry)
	if err := Atomic(filepath.Join(root, "registry.json"), before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureRuntime(root); err == nil {
		t.Fatal("unverified same-named adapters must be rejected")
	}
	after, _ := os.ReadFile(filepath.Join(root, "registry.json"))
	if !bytes.Equal(after, before) {
		t.Fatal("runtime refresh adopted a plugin it does not own")
	}
	if _, err := os.Stat(filepath.Join(root, "runtime", "builtin")); !os.IsNotExist(err) {
		t.Fatal("runtime configure installed an unregistered builtin bundle")
	}
}

func TestRuntimeErrorsLeaveExistingRegistryAndBindingIntact(t *testing.T) {
	root := runtimeFixture(t)
	host := []byte("{\"fixture\":\"existing host binding\"}\n")
	if err := Atomic(filepath.Join(root, "host.json"), host, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("{broken"), []byte("{\"version\":2,\"plugins\":[]}")} {
		if err := Atomic(filepath.Join(root, "registry.json"), bad, 0600); err != nil {
			t.Fatal(err)
		}
		if err := ConfigureRuntime(root); err == nil {
			t.Fatal("invalid registry should fail")
		}
		got, _ := os.ReadFile(filepath.Join(root, "registry.json"))
		gotHost, _ := os.ReadFile(filepath.Join(root, "host.json"))
		if !bytes.Equal(got, bad) || !bytes.Equal(gotHost, host) {
			t.Fatal("failed validation modified existing state")
		}
	}
	valid := []byte("{\"version\":1,\"plugins\":[]}")
	if err := Atomic(filepath.Join(root, "registry.json"), valid, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := ConfigureRuntime(root); err == nil {
		t.Fatal("missing Bun should fail explicitly")
	}
	got, _ := os.ReadFile(filepath.Join(root, "registry.json"))
	gotHost, _ := os.ReadFile(filepath.Join(root, "host.json"))
	if !bytes.Equal(got, valid) || !bytes.Equal(gotHost, host) {
		t.Fatal("missing runtime modified existing state")
	}
}
