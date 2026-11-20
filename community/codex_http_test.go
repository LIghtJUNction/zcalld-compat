package community

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexHTTPExplicitBindingsPersistAcrossRegistryChanges(t *testing.T) {
	root := t.TempDir()
	r := Registry{Version: 1, Plugins: []Entry{}, Accounts: map[string]AccountMeta{"builtin:codex#fixture": {Enabled: true}}}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bindings.json")
	data := []byte(`{"enabled":true,"loopback":{"provider":"codex-local","routes":["alias"]},"callers":{"lan-key":{"provider":"codex-lan","routes":["lan-alias"]}}}`)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexHTTP(root, "configure", file); err != nil {
		t.Fatal(err)
	}
	r, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.CodexHTTP == nil || !r.CodexHTTP.Enabled || r.CodexHTTP.Loopback.Provider != "codex-local" {
		t.Fatal("binding lost")
	}
	r.Accounts["builtin:codex#fixture"] = AccountMeta{Enabled: false}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	result, err := CodexHTTP(root, "show", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !json.Valid(raw) {
		t.Fatal("invalid result")
	}
	if _, err := CodexHTTP(root, "disable", ""); err != nil {
		t.Fatal(err)
	}
	r, _ = Read(root)
	if r.CodexHTTP.Enabled || r.CodexHTTP.Loopback == nil {
		t.Fatal("disable lost bindings")
	}
	if _, err := os.Stat(filepath.Join(root, "plugin-auth.json")); !os.IsNotExist(err) {
		t.Fatal("configuration touched credentials")
	}
	if _, err := os.Stat(filepath.Join(root, "host.json")); !os.IsNotExist(err) {
		t.Fatal("configuration started/bound a runtime")
	}
}
func TestCodexHTTPInvalidInputDoesNotChangeRegistry(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, Registry{Version: 1, Plugins: []Entry{}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	before, _ := os.ReadFile(path)
	cases := []string{
		"null",
		"{\"enabled\":null}",
		"{\"loopback\":{\"provider\":\"a\",\"routes\":[\"r\"]}}",
		`{"enabled":true}`,
		`{"enabled":true,"url":"https://untrusted","loopback":{"provider":"a","routes":["r"]}}`,
		`{"enabled":true,"loopback":{"provider":"a","routes":["r","r"]}}`,
		`{"enabled":true,"loopback":{"provider":"a","routes":["r"],"account":"guess"}}`,
		`{"enabled":true,"loopback":{"provider":"a","routes":["r"]}} {}`,
		`{"enabled":true,"callers":{"bad\\nkey":{"provider":"a","routes":[]}}}`,
	}
	for _, body := range cases {
		file := filepath.Join(t.TempDir(), "config.json")
		_ = os.WriteFile(file, []byte(body), 0600)
		if _, err := CodexHTTP(root, "configure", file); err == nil {
			t.Fatalf("accepted %s", body)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) {
			t.Fatal("invalid binding changed registry")
		}
	}
}
