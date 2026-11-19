package community

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeAccounts(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "extensions")
	r := Registry{Version: 1, Plugins: []Entry{}}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	auth := []byte("{\"fixture#one\":{\"type\":\"oauth\",\"access\":\"synthetic-one\",\"refresh\":\"synthetic-refresh\"},\"fixture#two\":{\"type\":\"api\",\"key\":\"synthetic-two\"}}")
	if err := Atomic(filepath.Join(root, "plugin-auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestAccountSelectionAndDisableUseLiveState(t *testing.T) {
	root := fakeAccounts(t)
	path := filepath.Join(root, "plugin-auth.json")
	before, _ := os.ReadFile(path)
	result, err := Accounts(root, []string{"list"}, AccountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "synthetic") {
		t.Fatal("public account list exposed credentials")
	}
	if _, err = Accounts(root, []string{"disable", "fixture#one"}, AccountOptions{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("disable moved or rewrote a rotating credential")
	}
	if _, err = Accounts(root, []string{"use", "fixture#one"}, AccountOptions{}); err == nil {
		t.Fatal("disabled account selected")
	}
	result, err = Accounts(root, []string{"use", "fixture#two"}, AccountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result["selection"].(map[string]any)["account"] != "fixture#two" {
		t.Fatal("runtime binding missing")
	}
	r, _ := Read(root)
	if r.AccountDefaults["fixture"] != "fixture#two" || r.Accounts["fixture#one"].Enabled {
		t.Fatal("runtime registry did not change")
	}
	if _, err = Accounts(root, []string{"remove", "fixture#one"}, AccountOptions{}); err != nil {
		t.Fatal(err)
	}
	all, _, _ := readAuthFile(root)
	if _, found := all["fixture#one"]; found {
		t.Fatal("remove left usable credentials")
	}
	if len(all) != 1 {
		t.Fatal("unrelated account changed")
	}
}
func TestAccountImportWaitsForConcurrentRefresh(t *testing.T) {
	root := fakeAccounts(t)
	lock := filepath.Join(root, "plugin-auth.json.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(private, []byte("{\"type\":\"oauth\",\"access\":\"new-private\",\"expires\":9007199254740993}"), 0600)
	done := make(chan error, 1)
	go func() {
		time.Sleep(60 * time.Millisecond)
		all, _, e := readAuthFile(root)
		if e == nil {
			all["fixture#one"].(map[string]any)["refresh"] = "rotated-before-import"
			b, _ := json.Marshal(all)
			e = Atomic(filepath.Join(root, "plugin-auth.json"), b, 0600)
		}
		_ = os.Remove(lock)
		done <- e
	}()
	result, err := Accounts(root, []string{"import"}, AccountOptions{Provider: "fixture", ID: "fixture#three", Label: "third", File: private})
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), "new-private") {
		t.Fatal("import exposed credentials")
	}
	all, _, err := readAuthFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if all["fixture#one"].(map[string]any)["refresh"] != "rotated-before-import" {
		t.Fatal("concurrent refresh was overwritten")
	}
	if all["fixture#three"].(map[string]any)["expires"].(json.Number).String() != "9007199254740993" {
		t.Fatal("credential number was rounded")
	}
}
func TestImportedInactiveBuiltinAccountStaysInactive(t *testing.T) {
	root := fakeAccounts(t)
	b := []byte("{\"accounts\":[{\"key\":\"fixture#one\",\"enabled\":false}]}")
	if err := Atomic(filepath.Join(root, "builtin-auth-map.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileAccounts(root); err != nil {
		t.Fatal(err)
	}
	r, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Accounts["fixture#one"].Enabled || r.AccountDefaults["fixture"] != "fixture#two" {
		t.Fatal("inactive imported account became active")
	}
}
