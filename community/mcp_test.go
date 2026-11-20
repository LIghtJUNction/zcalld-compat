package community

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPImportedSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"defaults", "", true}, {"all", "preset: all\n", true}, {"essential", "preset: essentials\n", true},
		{"off", "plugins:\n  - id: tools\n    enabled: false\n", false},
		{"flow", "plugins: [{id: tools, enabled: false}]\n", false},
		{"last", "plugins: [{id: tools, enabled: false}, {id: tools, enabled: true}]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.yaml != "" {
				if e := os.WriteFile(filepath.Join(dir, "plugins.yml"), []byte(tc.yaml), 0600); e != nil {
					t.Fatal(e)
				}
			}
			got, e := toolsEnabled(dir)
			if e != nil || got != tc.want {
				t.Fatalf("got %v %v", got, e)
			}
		})
	}
}
func TestMCPInUsePreservesEffectiveGate(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{"plugins.yml": "plugins: [{id: tools, enabled: false}]\n", "plugins.json": `{"plugins":[{"spec":"mock","off":false}]}`, "plugin-tools.json": `{"mock":["echo"]}`} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	got, e := toolsEnabled(dir)
	if e != nil || !got {
		t.Fatalf("effective gate %v %v", got, e)
	}
}
func TestMCPYAMLBoundsAndUnknowns(t *testing.T) {
	for _, text := range []string{"preset: unknown\n", "plugins: [broken\n", "plugins: &loop [*loop]\n"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "plugins.yml"), []byte(text), 0600)
		if _, e := toolsEnabled(dir); e == nil {
			t.Fatal("accepted invalid YAML")
		}
	}
}
func TestMCPPermissionImportKeepsNoCredentials(t *testing.T) {
	source, root := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(source, "caller-keys.json"), []byte(`[{"id":"parent","secret":"never-copy-this-secret","mcp":["magpie-tools/echo"],"maxConcurrency":1},{"id":"child","issuer":"parent","readOnly":true,"expires":"2030-01-01T00:00:00Z"}]`), 0600)
	result, e := ImportMCP(root, source)
	if e != nil {
		t.Fatal(e)
	}
	if result["caller_keys"] != 2 || result["enabled"] != true {
		t.Fatal("wrong metadata")
	}
	r, e := Read(root)
	if e != nil {
		t.Fatal(e)
	}
	if r.MCPPermissions["child"].Issuer != "parent" || !r.MCPPermissions["child"].ReadOnly || r.MCPPermissions["child"].ExpiresUnixMS == 0 {
		t.Fatal("lost restriction")
	}
	b, e := os.ReadFile(filepath.Join(root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		t.Fatal("bad JSON")
	}
	if string(b) == "" || containsMCPSecret(raw, "never-copy-this-secret") {
		t.Fatal("copied gateway credential")
	}
}
func containsMCPSecret(v any, secret string) bool {
	switch x := v.(type) {
	case string:
		return x == secret
	case []any:
		for _, e := range x {
			if containsMCPSecret(e, secret) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if containsMCPSecret(e, secret) {
				return true
			}
		}
	}
	return false
}
func TestMCPImportRefusesUnsupportedRestrictionBeforeSave(t *testing.T) {
	for _, text := range []string{`[{"id":"a","limit":{"requests":1}}]`, `[{"id":"a","issuer":"missing"}]`, `[{"id":"a","issuer":"a"}]`, `[{"id":"a","maxConcurrency":-1}]`} {
		source, root := t.TempDir(), t.TempDir()
		os.WriteFile(filepath.Join(source, "caller-keys.json"), []byte(text), 0600)
		if _, e := ImportMCP(root, source); e == nil {
			t.Fatal("relaxed unknown restriction")
		}
		if _, e := os.Stat(filepath.Join(root, "registry.json")); !os.IsNotExist(e) {
			t.Fatal("wrote partial permission state")
		}
	}
}
