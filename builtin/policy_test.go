package builtin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LIghtJUNction/zcalld-compat/community"
)

func policyFixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "magpie")
	root := filepath.Join(dir, "extensions")
	key := "builtin:codex#opaque-fixture"
	user := " Fixture@Example.Invalid "
	put(t, filepath.Join(source, "logins.json"), []object{
		{"id": "login-a", "agent": "codex", "user": user, "auth": object{"access": "never-read-or-returned"}},
	})
	put(t, filepath.Join(source, "settings.json"), object{"codexNoCredits": []string{"fixture@example.invalid"}})
	absolute, err := canonical(source)
	if err != nil {
		t.Fatal(err)
	}
	source = absolute
	put(t, filepath.Join(root, "builtin-auth-map.json"), Mapping{
		Version: 1, Source: source, Accounts: []Account{
			{Key: key, Agent: "codex", Enabled: true, ImportedHash: "old-credential-fingerprint",
				Sources: []Source{{Path: filepath.Join(source, "logins.json"), Kind: "logins",
					LoginID: "login-a", UserHash: hash(user), SecretHash: "old-source-secret"}}},
		},
	})
	if err = community.Save(root, community.Registry{Version: 1, Plugins: []community.Entry{},
		Accounts: map[string]community.AccountMeta{key: {Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	// This is deliberately not JSON: policy operations must never read credentials.
	if err = os.WriteFile(filepath.Join(root, "plugin-auth.json"), []byte("opaque credentials\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, source, key
}

func policyBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexPolicyPlanAndSyncNeverTouchCredentials(t *testing.T) {
	root, source, key := policyFixture(t)
	registryBefore := policyBytes(t, filepath.Join(root, "registry.json"))
	mappingBefore := policyBytes(t, filepath.Join(root, "builtin-auth-map.json"))
	plan, err := CodexPolicy(root, source, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan["read_only"] != true || plan["matched_accounts"] != 1 || plan["enable"] != 1 ||
		plan["restricted_accounts"] != 1 || plan["blocked"] != false {
		t.Fatalf("unexpected aggregate plan: %#v", plan)
	}
	if _, err = os.Stat(filepath.Join(root, ".lock")); !os.IsNotExist(err) {
		t.Fatal("read-only plan created a lock")
	}
	if !bytes.Equal(registryBefore, policyBytes(t, filepath.Join(root, "registry.json"))) ||
		!bytes.Equal(mappingBefore, policyBytes(t, filepath.Join(root, "builtin-auth-map.json"))) {
		t.Fatal("plan changed state")
	}
	// The native owner can rotate tokens independently without invalidating policy.
	rotated := []byte("newer opaque credentials\n")
	if err = os.WriteFile(filepath.Join(root, "plugin-auth.json"), rotated, 0600); err != nil {
		t.Fatal(err)
	}
	var mapping Mapping
	if err = read(filepath.Join(root, "builtin-auth-map.json"), &mapping); err != nil {
		t.Fatal(err)
	}
	mapping.Accounts[0].ImportedHash = "new-fingerprint"
	mapping.Accounts[0].Sources[0].SecretHash = "new-source-fingerprint"
	put(t, filepath.Join(root, "builtin-auth-map.json"), mapping)
	result, err := CodexPolicy(root, source, plan["plan_hash"].(string), true)
	if err != nil {
		t.Fatal(err)
	}
	if result["credentials_changed"] != false || result["restart_required"] != false {
		t.Fatal("policy sync changed credential ownership or requires a stale loader restart")
	}
	registry, err := community.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := policyValue(registry.AccountPolicies[key])
	if err != nil || !restricted {
		t.Fatal("policy did not follow the proved source login identity")
	}
	if !bytes.Equal(rotated, policyBytes(t, filepath.Join(root, "plugin-auth.json"))) {
		t.Fatal("policy synchronization overwrote rotated credentials")
	}
	var preserved Mapping
	if err = read(filepath.Join(root, "builtin-auth-map.json"), &preserved); err != nil ||
		preserved.Accounts[0].ImportedHash != "new-fingerprint" ||
		preserved.Accounts[0].Sources[0].SecretHash != "new-source-fingerprint" {
		t.Fatal("policy synchronization rewrote migration fingerprints")
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "fixture@") || strings.Contains(string(data), key) ||
		strings.Contains(string(data), "fingerprint") || strings.Contains(string(data), "never-read") {
		t.Fatal("policy result leaked account data")
	}
}

func TestCodexPolicyRejectsDriftAndCanExplicitlyRemoveRestriction(t *testing.T) {
	root, source, key := policyFixture(t)
	plan, err := CodexPolicy(root, source, "", false)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := community.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	registry.Accounts[key] = community.AccountMeta{Enabled: false}
	if err = community.Save(root, registry); err != nil {
		t.Fatal(err)
	}
	before := policyBytes(t, filepath.Join(root, "registry.json"))
	if _, err = CodexPolicy(root, source, plan["plan_hash"].(string), true); err == nil {
		t.Fatal("stale plan ignored a changed target account")
	}
	if !bytes.Equal(before, policyBytes(t, filepath.Join(root, "registry.json"))) {
		t.Fatal("rejected synchronization changed the registry")
	}
	plan, err = CodexPolicy(root, source, "", false)
	if err != nil || plan["enabled_accounts"] != 0 {
		t.Fatalf("disabled account plan: %v", err)
	}
	if _, err = CodexPolicy(root, source, plan["plan_hash"].(string), true); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(source, "settings.json"), object{"codexNoCredits": []string{}})
	plan, err = CodexPolicy(root, source, "", false)
	if err != nil || plan["disable"] != 1 {
		t.Fatalf("explicit removal plan: %v", err)
	}
	if _, err = CodexPolicy(root, source, plan["plan_hash"].(string), true); err != nil {
		t.Fatal(err)
	}
	registry, _ = community.Read(root)
	if _, exists := registry.AccountPolicies[key]; exists {
		t.Fatal("explicit removal retained the restriction")
	}
}

func TestCodexPolicyFailsClosedOnUnmappedOrChangedIdentity(t *testing.T) {
	t.Run("unmapped", func(t *testing.T) {
		root, source, _ := policyFixture(t)
		put(t, filepath.Join(source, "settings.json"), object{"codexNoCredits": []string{"unknown@example.invalid"}})
		plan, err := CodexPolicy(root, source, "", false)
		if err != nil || plan["unmapped_policy_users"] != 1 || plan["blocked"] != true {
			t.Fatalf("unmapped source policy was not blocked: %v", err)
		}
		before := policyBytes(t, filepath.Join(root, "registry.json"))
		if _, err = CodexPolicy(root, source, plan["plan_hash"].(string), true); err == nil {
			t.Fatal("unmapped restriction was silently discarded")
		}
		if !bytes.Equal(before, policyBytes(t, filepath.Join(root, "registry.json"))) {
			t.Fatal("blocked plan changed registry")
		}
	})
	for _, name := range []string{"reused-id", "ambiguous-id"} {
		t.Run(name, func(t *testing.T) {
			root, source, _ := policyFixture(t)
			logins := []object{{"id": "login-a", "agent": "codex", "user": "different@example.invalid"}}
			if name == "ambiguous-id" {
				logins = []object{{"id": "login-a", "agent": "codex", "user": " Fixture@Example.Invalid "},
					{"id": "login-a", "agent": "codex", "user": " Fixture@Example.Invalid "}}
			}
			put(t, filepath.Join(source, "logins.json"), logins)
			if _, err := CodexPolicy(root, source, "", false); err == nil {
				t.Fatal("ambiguous or reused login ID was accepted")
			}
		})
	}
}

func TestCodexPolicyInvalidPresentPolicyIsNeverUnrestricted(t *testing.T) {
	for _, raw := range []string{"null", "{}", "true", `{"codex_no_credits":null}`,
		`{"codex_no_credits":"true"}`, `{"codex_no_credits":true,"unknown":1}`} {
		t.Run(raw, func(t *testing.T) {
			root, source, key := policyFixture(t)
			registry, _ := community.Read(root)
			registry.AccountPolicies = map[string]json.RawMessage{key: json.RawMessage(raw)}
			if err := community.Save(root, registry); err != nil {
				t.Fatal(err)
			}
			if _, err := CodexPolicy(root, source, "", false); err == nil {
				t.Fatal("invalid present policy became unrestricted")
			}
		})
	}
}

func TestCodexPolicyDoesNotGuessNativeOnlyAccountIdentity(t *testing.T) {
	root, source, _ := policyFixture(t)
	var mapping Mapping
	if err := read(filepath.Join(root, "builtin-auth-map.json"), &mapping); err != nil {
		t.Fatal(err)
	}
	mapping.Accounts[0].Sources = []Source{{Kind: "codex", Path: filepath.Join(t.TempDir(), "auth.json")}}
	put(t, filepath.Join(root, "builtin-auth-map.json"), mapping)
	plan, err := CodexPolicy(root, source, "", false)
	if err != nil || plan["matched_accounts"] != 0 || plan["blocked"] != true {
		t.Fatalf("native-only account was guessed from its key: %#v %v", plan, err)
	}
}
