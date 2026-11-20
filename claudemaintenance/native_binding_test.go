package claudemaintenance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

func nativeFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t, map[string]any{}, true)
	if err := os.RemoveAll(f.source); err != nil {
		t.Fatal(err)
	} // owned synthetic source deliberately absent
	f.bindingsValue.Accounts[0].LoginID = ""
	f.bindingsValue.Accounts[0].UserHash = ""
	protectWrite(t, f.bindings, f.bindingsValue)
	return f
}

func bindNative(t *testing.T, f fixture) string {
	t.Helper()
	p, err := NativeBindingPlan(t.Context(), f.root, f.bindings, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Accounts != 1 || p.AutomaticAccounts != 0 || p.Applied {
		t.Fatal(p)
	}
	a, err := NativeBindingPlan(t.Context(), f.root, f.bindings, p.PlanHash, true)
	if err != nil || !a.Applied || a.CredentialsImported || a.SchedulerChanged {
		t.Fatal(a, err)
	}
	return a.AccountIDs[0]
}

func TestNativeBindingNeedsNoMagpieAndDefaultsToNoExtraWithoutSchedule(t *testing.T) {
	f := nativeFixture(t)
	before, err := os.ReadFile(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	key := bindNative(t, f)
	doc, err := Store(f.root).Read()
	if err != nil {
		t.Fatal(err)
	}
	c, err := readConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Accounts[key]
	p := doc.Policy.Accounts[key]
	if !a.NoExtra || a.Automatic || doc.Policy.Enabled || p.WeeklyWarmup || p.FiveHourWarmup || len(p.WarmAts) != 0 {
		t.Fatal("binding enabled automatic or paid behavior")
	}
	after, _ := os.ReadFile(f.profile)
	if string(before) != string(after) || len(generations(t, f)) != 0 {
		t.Fatal("binding changed profile or launched CLI")
	}
	if _, err = os.Stat(f.source); !os.IsNotExist(err) {
		t.Fatal("binding recreated source metadata")
	}
	// Explicit all-account warmup includes a deliberately bound native account,
	// without turning on its timer policy or requiring a legacy on flag.
	report, err := Manual(t.Context(), f.root, "", true)
	if err != nil || len(report.Results) != 1 {
		t.Fatal("native binding missing from explicit all action", report, err)
	}
	afterDoc, err := Store(f.root).Read()
	if err != nil || afterDoc.Policy.Enabled {
		t.Fatal("manual all enabled scheduling", err)
	}
}

func TestNativeBindingRejectsStaleStateAndIdentityBeforeWriting(t *testing.T) {
	f := nativeFixture(t)
	_ = bindNative(t, f)
	p, err := NativeBindingPlan(t.Context(), f.root, f.bindings, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = Store(f.root).SetEnabled(t.Context(), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = NativeBindingPlan(t.Context(), f.root, f.bindings, p.PlanHash, true); err == nil {
		t.Fatal("stale state accepted")
	}
	p, err = NativeBindingPlan(t.Context(), f.root, f.bindings, "", false)
	if err != nil {
		t.Fatal(err)
	}
	protectWrite(t, f.profile, map[string]any{"oauthAccount": identity{Email: syntheticUser, Organization: "changed", Account: "fake-member"}})
	if _, err = NativeBindingPlan(t.Context(), f.root, f.bindings, p.PlanHash, true); err == nil {
		t.Fatal("changed identity accepted")
	}
}

func TestNativeBindingKeepsExistingPendingLedgerAndPreferences(t *testing.T) {
	f := nativeFixture(t)
	key := bindNative(t, f)
	doc, err := Store(f.root).Read()
	if err != nil {
		t.Fatal(err)
	}
	doc.Policy.Enabled = true
	doc.Policy.EnabledAt = time.Now().UTC()
	doc.Policy.Accounts[key] = maintenance.AccountPolicy{WeeklyWarmup: true, ResetLeadMinutes: 30}
	doc.Ledger[key].Warm = &maintenance.WarmIntent{Phase: "uncertain", Windows: []string{"7 days"}, PreparedAt: time.Now().UTC()}
	var c config
	if json.Unmarshal(doc.Adapter, &c) != nil {
		t.Fatal("config")
	}
	a := c.Accounts[key]
	a.Automatic = true
	a.ExtraAmount = 12.5
	c.Accounts[key] = a
	doc.Adapter, _ = json.Marshal(c)
	protectWrite(t, filepath.Join(Store(f.root).Directory, "state.json"), doc)
	before, _ := json.Marshal(doc.Ledger)
	p, err := NativeBindingPlan(t.Context(), f.root, f.bindings, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NativeBindingPlan(t.Context(), f.root, f.bindings, p.PlanHash, true); err != nil {
		t.Fatal(err)
	}
	after, err := Store(f.root).Read()
	if err != nil {
		t.Fatal(err)
	}
	ledger, _ := json.Marshal(after.Ledger)
	c, err = readConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(ledger) || !after.Policy.Enabled || !after.Policy.Accounts[key].WeeklyWarmup || !c.Accounts[key].Automatic || c.Accounts[key].ExtraAmount != 12.5 {
		t.Fatal("binding changed existing policy or unknown ledger")
	}
}
