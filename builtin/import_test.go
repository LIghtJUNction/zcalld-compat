package builtin

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixtureJWT() string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`)) + ".signature"
}
func put(t *testing.T, path string, v any) {
	t.Helper()
	if err := save(path, v); err != nil {
		t.Fatal(err)
	}
}
func setup(t *testing.T) (root, source string, paths activePaths) {
	t.Helper()
	dir := t.TempDir()
	root = filepath.Join(dir, "new", "extensions")
	source = filepath.Join(dir, "magpie")
	paths = activePaths{CodexHome: filepath.Join(dir, "codex"), ConfigHome: filepath.Join(dir, "config")}
	old := object{"auth_mode": "chatgpt", "keep": "untouched", "tokens": object{"access_token": fixtureJWT(), "refresh_token": "fixture-refresh-old", "id_token": "fixture-id-old", "account_id": "fixture-account"}}
	put(t, filepath.Join(source, "logins.json"), []object{
		{"agent": "codex", "id": "fixture-codex-login", "user": "fixture-codex", "auth": old},
		{"agent": "copilot", "id": "fixture-copilot-login", "user": "fixture-copilot", "auth": nil},
		{"agent": "claude", "id": "fixture-claude-login", "user": "fixture-claude", "lapsed": "fixture", "auth": nil},
	})
	put(t, filepath.Join(paths.CodexHome, "auth.json"), old)
	put(t, filepath.Join(paths.ConfigHome, "github-copilot", "apps.json"), object{"github.com:fixture-app": object{"user": "fixture-copilot", "oauth_token": "fixture-github-token", "scopes": []string{"read:user"}}})
	put(t, filepath.Join(root, "plugin-auth.json"), object{"community-fixture": object{"type": "api", "key": "fixture-community-key"}})
	return
}

func TestImportAndExplicitRotationRollback(t *testing.T) {
	root, source, paths := setup(t)
	before, err := os.ReadFile(filepath.Join(source, "logins.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importPaths(root, source, paths)
	if err != nil {
		t.Fatal(err)
	}
	if result["builtin_accounts"] != 2 || result["enabled_accounts"] != 2 {
		t.Fatalf("unexpected account counts: %#v", result)
	}
	after, _ := os.ReadFile(filepath.Join(source, "logins.json"))
	if string(before) != string(after) {
		t.Fatal("import modified the original gateway")
	}
	var mapping Mapping
	if err = read(filepath.Join(root, "builtin-auth-map.json"), &mapping); err != nil {
		t.Fatal(err)
	}
	var auth object
	if err = read(filepath.Join(root, "plugin-auth.json"), &auth); err != nil {
		t.Fatal(err)
	}
	if str(obj(auth["community-fixture"])["key"]) != "fixture-community-key" {
		t.Fatal("import lost community credential")
	}
	key := ""
	for _, a := range mapping.Accounts {
		if a.Agent == "codex" {
			key = a.Key
			if len(a.Sources) != 2 {
				t.Fatal("Codex original and active sources were not both recorded")
			}
		}
	}
	if !strings.HasPrefix(key, "builtin:codex#") {
		t.Fatal("missing scoped Codex key")
	}
	if obj(auth[key])["refreshOwner"] != "external" {
		t.Fatal("active Codex must retain its native refresh owner")
	}
	if runtime.GOOS != "linux" {
		t.Skip("explicit native owner handoff verification is Linux-only")
	}
	claimFixture(t, root, paths, key)
	if err = read(filepath.Join(root, "plugin-auth.json"), &auth); err != nil {
		t.Fatal(err)
	}
	entry := obj(auth[key])
	entry["access"] = "fixture-access-new"
	entry["refresh"] = "fixture-refresh-new"
	entry["idToken"] = "fixture-id-new"
	entry["expires"] = int64(4102444800000)
	put(t, filepath.Join(root, "plugin-auth.json"), auth)
	if _, err = importPaths(root, source, paths); err == nil {
		t.Fatal("reimport overwrote a token rotated by the new owner")
	}
	if _, err = Sync(root, source); err != nil {
		t.Fatal(err)
	}
	var active object
	if err = read(filepath.Join(paths.CodexHome, "auth.json"), &active); err != nil {
		t.Fatal(err)
	}
	if str(obj(active["tokens"])["refresh_token"]) != "fixture-refresh-new" || active["keep"] != "untouched" {
		t.Fatal("active Codex token fields were not safely synchronized")
	}
	var logins []object
	if err = read(filepath.Join(source, "logins.json"), &logins); err != nil {
		t.Fatal(err)
	}
	if str(obj(obj(logins[0]["auth"])["tokens"])["refresh_token"]) != "fixture-refresh-new" {
		t.Fatal("saved Codex token was not synchronized")
	}
	if logins[1]["auth"] != nil || logins[2]["lapsed"] != "fixture" {
		t.Fatal("sync changed unrelated login metadata")
	}
	if _, err = Sync(root, source); err != nil {
		t.Fatalf("repeat sync must be idempotent: %v", err)
	}
	for _, name := range []string{"plugin-auth.json", "builtin-auth-map.json", "builtin-providers.json"} {
		st, e := os.Stat(filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		if st.Mode().Perm()&0077 != 0 {
			t.Fatalf("%s is not private", name)
		}
	}
	mapBytes, _ := os.ReadFile(filepath.Join(root, "builtin-auth-map.json"))
	if strings.Contains(string(mapBytes), "fixture-refresh") {
		t.Fatal("mapping should store fingerprints, not refresh tokens")
	}
	var catalog []object
	if err = read(filepath.Join(root, "builtin-providers.json"), &catalog); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range catalog {
		if str(p["accountId"]) == "" {
			t.Fatal("catalog omitted account binding")
		}
		if models, ok := p["models"].([]any); ok {
			count += len(models)
		}
	}
	if count != 34 {
		t.Fatalf("expected all 34 imported Codex/Copilot models, got %d", count)
	}
}

func TestRollbackRefusesDifferentOwnerAndPrevalidatesAllSources(t *testing.T) {
	root, source, paths := setup(t)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	if runtime.GOOS != "linux" {
		t.Skip("explicit native owner handoff verification is Linux-only")
	}
	for key := range auth {
		if strings.HasPrefix(key, "builtin:codex#") {
			claimFixture(t, root, paths, key)
		}
	}
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	for key, value := range auth {
		if strings.HasPrefix(key, "builtin:codex#") {
			obj(value)["access"] = "fixture-new-access"
			obj(value)["refresh"] = "fixture-new-refresh"
		}
	}
	put(t, filepath.Join(root, "plugin-auth.json"), auth)
	var active object
	read(filepath.Join(paths.CodexHome, "auth.json"), &active)
	obj(active["tokens"])["refresh_token"] = "fixture-another-owner"
	put(t, filepath.Join(paths.CodexHome, "auth.json"), active)
	before, _ := os.ReadFile(filepath.Join(source, "logins.json"))
	if _, err := Sync(root, source); err == nil || !strings.Contains(err.Error(), "another owner") {
		t.Fatalf("expected fingerprint conflict, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(source, "logins.json"))
	if string(before) != string(after) {
		t.Fatal("sync wrote one source before checking the others")
	}
}

func TestStagedReimportReadsNewSourceWithoutNetwork(t *testing.T) {
	root, source, paths := setup(t)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var active object
	read(filepath.Join(paths.CodexHome, "auth.json"), &active)
	obj(active["tokens"])["refresh_token"] = "fixture-latest-source-refresh"
	put(t, filepath.Join(paths.CodexHome, "auth.json"), active)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	for key, value := range auth {
		if strings.HasPrefix(key, "builtin:codex#") && obj(value)["refresh"] != "fixture-latest-source-refresh" {
			t.Fatal("handoff reimport did not take the active source")
		}
	}
}

func TestBusyCredentialFileAndMalformedInput(t *testing.T) {
	root, source, paths := setup(t)
	lock := filepath.Join(root, "plugin-auth.json.lock")
	if err := os.WriteFile(lock, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importPaths(root, source, paths); err == nil {
		t.Fatal("import ignored another writer's auth lock")
	}
	os.Remove(lock)
	if err := os.WriteFile(filepath.Join(source, "logins.json"), []byte(`[] {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importPaths(root, source, paths); err == nil {
		t.Fatal("import accepted trailing credential JSON")
	}
}

func TestStringAuthShapeIsPreserved(t *testing.T) {
	root, source, paths := setup(t)
	var logins []object
	read(filepath.Join(source, "logins.json"), &logins)
	b, _ := json.Marshal(logins[0]["auth"])
	logins[0]["auth"] = string(b)
	put(t, filepath.Join(source, "logins.json"), logins)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		t.Skip("explicit native owner handoff verification is Linux-only")
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	for key := range auth {
		if strings.HasPrefix(key, "builtin:codex#") {
			claimFixture(t, root, paths, key)
		}
	}
	if _, err := Sync(root, source); err != nil {
		t.Fatal(err)
	}
	read(filepath.Join(source, "logins.json"), &logins)
	if _, ok := logins[0]["auth"].(string); !ok {
		t.Fatal("rollback changed a string auth field into an object")
	}
}

func TestPrivateGoogleClientIdentityIsCopiedWithoutPublication(t *testing.T) {
	root, source, paths := setup(t)
	var logins []object
	read(filepath.Join(source, "logins.json"), &logins)
	logins = append(logins, object{"agent": "antigravity", "id": "fixture-google-login", "user": "fixture-google", "on": true, "auth": object{"access_token": "fixture-google-access", "refresh_token": "fixture-google-refresh", "expiry_date": 1}})
	put(t, filepath.Join(source, "logins.json"), logins)
	result, err := importPaths(root, source, paths)
	if err != nil {
		t.Fatal(err)
	}
	if result["google_client_setup_required"] != 1 {
		t.Fatal("missing Google app identity was not reported")
	}
	put(t, filepath.Join(root, "builtin-oauth-clients.json"), object{"antigravity": object{"clientId": "fixture-client-id", "clientSecret": "fixture-client-secret"}})
	result, err = importPaths(root, source, paths)
	if err != nil {
		t.Fatal(err)
	}
	if result["google_client_setup_required"] != 0 {
		t.Fatal("private app identity was not loaded")
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	for key, value := range auth {
		if strings.HasPrefix(key, "builtin:antigravity#") && obj(value)["clientSecret"] != "fixture-client-secret" {
			t.Fatal("private identity not carried with Google refresh token")
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, "builtin-providers.json"))
	if strings.Contains(string(data), "fixture-client-secret") {
		t.Fatal("OAuth client identity leaked into catalog")
	}
}
