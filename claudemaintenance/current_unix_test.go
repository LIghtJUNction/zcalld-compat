//go:build !windows

package claudemaintenance

import (
	"os"
	"testing"
)

func TestCurrentDirectoryAllowsNativeReadModeWithoutChangingIt(t *testing.T) {
	f := newFixture(t, map[string]any{"claudeWarmup": "off"}, true)
	if err := os.Chmod(f.config, 0755); err != nil {
		t.Fatal(err)
	}
	_ = f.apply(t)
	i, err := os.Stat(f.config)
	if err != nil || i.Mode().Perm() != 0755 {
		t.Fatal("native directory mode was changed")
	}
	if err = os.Chmod(f.config, 0775); err != nil {
		t.Fatal(err)
	}
	if _, err = Plan(t.Context(), f.root, f.source, f.bindings, "", false); err == nil {
		t.Fatal("writable current directory accepted")
	}
	if err = os.Chmod(f.config, 0755); err != nil {
		t.Fatal(err)
	}
	f.bindingsValue.Accounts[0].Ownership = "saved"
	protectWrite(t, f.bindings, f.bindingsValue)
	if _, err = Plan(t.Context(), f.root, f.source, f.bindings, "", false); err == nil {
		t.Fatal("nonprivate saved directory accepted")
	}
}
