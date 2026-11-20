package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/community"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

const maintenanceAccount = "builtin:codex#0123456789abcdefabcd"

func maintenanceFixture(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "extensions")
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	original := filepath.Join(t.TempDir(), "original")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	data := map[string]string{"settings.json": `{"codexWarmup":"week","codexAutoReset":["user@example.invalid"],"codexAutoResetOnly":{"user@example.invalid":"expiring"}}`, "logins.json": `[{"agent":"codex","id":"source-id","user":"User@example.invalid","secret":"never-copy-this"}]`, "codex-warmup.json": `{"user@example.invalid":{"7 days":{"used":17}}}`}
	for name, value := range data {
		if err := os.WriteFile(filepath.Join(snapshot, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := Mapping{Version: 1, Source: original, Accounts: []Account{{Key: maintenanceAccount, Agent: "codex", Enabled: true, Sources: []Source{{Kind: "logins", Path: filepath.Join(original, "logins.json"), LoginID: "source-id", UserHash: hash("User@example.invalid")}}}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(root, "builtin-auth-map.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	var registry community.Registry
	if err := json.Unmarshal([]byte(`{"version":1,"plugins":[],"accounts":{"`+maintenanceAccount+`":{"enabled":true}},"account_policies":{"`+maintenanceAccount+`":{"codex_no_credits":true}}}`), &registry); err != nil {
		t.Fatal(err)
	}
	if err := community.Save(root, registry); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin-auth.json"), []byte("credential-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, snapshot
}
func TestMaintenanceSnapshotPlanBindsFilesAndDoesNotCopyCredentials(t *testing.T) {
	root, snapshot := maintenanceFixture(t)
	plan, err := MaintenancePlan(root, snapshot, "", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	for _, secret := range []string{"user@example", "never-copy", "credential-canary"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("plan leaked source content")
		}
	}
	h := plan["plan_hash"].(string)
	if _, err = MaintenancePlan(root, snapshot, h, true); err != nil {
		t.Fatal(err)
	}
	s := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	status, err := s.Status()
	if err != nil || status.Enabled || status.Accounts != 1 {
		t.Fatal(status, err)
	}
	if err = s.SetEnabled(context.Background(), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = MaintenancePlan(root, snapshot, h, true); err != nil {
		t.Fatal(err)
	}
	status, _ = s.Status()
	if !status.Enabled {
		t.Fatal("same import disabled configured maintenance")
	}
	unchanged, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	if string(unchanged) != "credential-canary" {
		t.Fatal("credential file changed")
	}
	state, _ := os.ReadFile(filepath.Join(root, "maintenance", "state.json"))
	if strings.Contains(string(state), "never-copy") {
		t.Fatal("snapshot login credentials imported")
	}
}
func TestMaintenanceChangedSnapshotOrEnabledStateInvalidatesPlan(t *testing.T) {
	for _, which := range []string{"settings", "ledger", "login", "mapping", "enabled"} {
		t.Run(which, func(t *testing.T) {
			root, snapshot := maintenanceFixture(t)
			p, err := MaintenancePlan(root, snapshot, "", false)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "enabled":
				r, _ := community.Read(root)
				meta := r.Accounts[maintenanceAccount]
				meta.Enabled = false
				r.Accounts[maintenanceAccount] = meta
				if err := community.Save(root, r); err != nil {
					t.Fatal(err)
				}
			case "mapping":
				var m Mapping
				if err := read(filepath.Join(root, "builtin-auth-map.json"), &m); err != nil {
					t.Fatal(err)
				}
				m.Accounts[0].Sources[0].LoginID = "changed"
				b, _ := json.Marshal(m)
				if err := os.WriteFile(filepath.Join(root, "builtin-auth-map.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				name := map[string]string{"settings": "settings.json", "ledger": "codex-warmup.json", "login": "logins.json"}[which]
				f, err := os.OpenFile(filepath.Join(snapshot, name), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = MaintenancePlan(root, snapshot, p["plan_hash"].(string), true); err == nil {
				t.Fatal("changed input retained old authority")
			}
			if _, err = os.Stat(filepath.Join(root, "maintenance", "state.json")); !os.IsNotExist(err) {
				t.Fatal("failed plan changed state")
			}
		})
	}
}
