package community

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

const maintenanceFixtureAccount = "builtin:codex#0123456789abcdefabcd"

func maintenanceHostFixture(t *testing.T, mode string) (string, maintenance.Store) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("real maintenance host fixture requires Bun")
	}
	root := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(name, filepath.Join(root, "isolated-home"))
	}
	entries, err := BuiltinScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(filepath.Dir(entries["codex.mjs"]), "maintenance-fixture.mjs")
	script := `import {createCodex} from './codex-impl.mjs';
import {appendFileSync,readFileSync} from 'node:fs';
globalThis.fetch=async()=>{throw new Error('UNEXPECTED-NETWORK')};
const reply=value=>new Response(JSON.stringify(value),{headers:{'content-type':'application/json'}});
export default async(input)=>{
appendFileSync(input.directory+'/loads','1');
let usageReads=0;
return createCodex({fetcher:async(url,init)=>{
 const u=String(url),h=new Headers(init.headers);
 if(h.get('chatgpt-account-id')!=='fixture-only-account')throw new Error('wrong explicit identity');
 if(Object.hasOwn(init,'zcalldAccountPolicy'))throw new Error('policy escaped transport');
 if(u==='https://chatgpt.com/backend-api/wham/usage'){
   usageReads++;
   const deny=readFileSync(input.directory+'/mode','utf8')==='guard-denies'&&usageReads>=2;
   return reply({rate_limit:{allowed:!deny,limit_reached:deny,
     primary_window:{used_percent:deny?100:20,limit_window_seconds:18000,reset_at:Math.floor(Date.now()/1000)+3600},
     secondary_window:{used_percent:0,limit_window_seconds:604800,reset_at:Math.floor(Date.now()/1000)+604800}},rate_limit_reset_credits:{available_count:0}});
 }
 if(u.startsWith('https://chatgpt.com/backend-api/codex/models?'))return reply({models:[{slug:'fixture-mini',supported_reasoning_levels:[{effort:'low'}]}]});
 if(u==='https://chatgpt.com/backend-api/codex/responses'){
   const b=JSON.parse(init.body);
   if(init.method!=='POST'||b.model!=='fixture-mini'||b.input?.length!==1||b.input[0]?.content?.[0]?.text!=='hi'||b.stream!==true)throw new Error('warmup input changed');
   appendFileSync(input.directory+'/generated','1');
   return new Response('event: response.completed\ndata: '+JSON.stringify({type:'response.completed',response:{id:'fixture',object:'response',status:'completed',output:[]}})+'\n\n',{headers:{'content-type':'text/event-stream'}});
 }
 throw new Error('unexpected mock route');
}});
};`
	if err = Atomic(wrapper, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Atomic(filepath.Join(root, "mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	var r Registry
	raw, _ := json.Marshal(map[string]any{"version": 1, "plugins": []any{map[string]any{"name": "builtin-codex", "kind": "community", "source": "builtin-codex", "path": wrapper, "enabled": true}}, "accounts": map[string]any{maintenanceFixtureAccount: map[string]any{"enabled": true}}, "account_policies": map[string]any{maintenanceFixtureAccount: map[string]any{"codex_no_credits": true}}})
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if err = Save(root, r); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "fixture-only-account"}})
	token := "fixture." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	auth, _ := json.Marshal(map[string]any{maintenanceFixtureAccount: map[string]any{"type": "oauth", "access": token, "refreshOwner": "host", "accountId": "fixture-only-account", "expires": time.Now().Add(time.Hour).UnixMilli()}, "builtin:codex#ffffffffffffffffffff": map[string]any{"type": "oauth", "access": "UNRELATED-UNCHANGED", "expires": 4102444800000}})
	if err = Atomic(filepath.Join(root, "plugin-auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := maintenance.PlanImport([]byte(`{"codexWarmup":"week"}`), []byte(`{}`), []maintenance.AccountLink{{SourceUser: "synthetic@example.invalid", Account: maintenanceFixtureAccount, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	if err = store.ApplyPlan(t.Context(), plan, plan.Hash, true, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return root, store
}

func TestMaintenanceRealHostUsesNormalLoaderAndDoesNotRepeat(t *testing.T) {
	root, store := maintenanceHostFixture(t, "success")
	auth, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	registry, _ := os.ReadFile(filepath.Join(root, "registry.json"))
	result, err := MaintenanceRun(t.Context(), root, "")
	if err != nil || len(result.Results) != 1 || result.Results[0].Status != "warmup_completed" {
		t.Fatal(result, err)
	}
	generated, _ := os.ReadFile(filepath.Join(root, "generated"))
	if string(generated) != "1" {
		t.Fatal("expected one fixed synthetic generation")
	}
	if _, err = MaintenanceRun(t.Context(), root, ""); err != nil {
		t.Fatal(err)
	}
	loads, _ := os.ReadFile(filepath.Join(root, "loads"))
	if string(loads) != "1" {
		t.Fatal("not-due run started another host")
	}
	current, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	if string(current) != string(auth) {
		t.Fatal("maintenance changed unrelated credentials")
	}
	current, _ = os.ReadFile(filepath.Join(root, "registry.json"))
	if string(current) != string(registry) {
		t.Fatal("maintenance rewrote registry")
	}
	state, err := store.Read()
	if err != nil || state.Ledger[maintenanceFixtureAccount].Warm != nil {
		t.Fatal("completed request not recorded", err)
	}
}

func TestMaintenanceRealHostRechecksNoCreditsAtGeneration(t *testing.T) {
	root, _ := maintenanceHostFixture(t, "guard-denies")
	deadline := time.Now().Add(5 * time.Second)
	host, err := startHostBudget(root, "builtin-codex", deadline, deadline.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	var result maintenance.ActionResult
	err = host.callUntil("maintenance.warmup", map[string]any{"account": maintenanceFixtureAccount, "windows": []string{"secondary"}, "deadline_ms": deadline.UnixMilli()}, &result, deadline)
	if err != nil || result.Status != "not_sent" || result.Code != "policy_blocked" {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(root, "generated")); !os.IsNotExist(err) {
		t.Fatal("warmup bypassed the normal NoCredits guard")
	}
}

func TestMaintenanceRealHostRejectsImplicitAccountAndPolicyInputs(t *testing.T) {
	root, _ := maintenanceHostFixture(t, "success")
	deadline := time.Now().Add(5 * time.Second)
	host, err := startHostBudget(root, "builtin-codex", deadline, deadline.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	for _, params := range []map[string]any{
		{"deadline_ms": deadline.UnixMilli()},
		{"account": maintenanceFixtureAccount, "deadline_ms": deadline.UnixMilli(), "url": "https://unrelated.invalid/"},
		{"account": maintenanceFixtureAccount, "deadline_ms": deadline.UnixMilli(), "no_credits": false},
	} {
		var out any
		if err = host.callUntil("maintenance.inspect", params, &out, deadline); err == nil {
			t.Fatal("accepted external authority override")
		}
	}
	r, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	m := r.Accounts[maintenanceFixtureAccount]
	m.Enabled = false
	r.Accounts[maintenanceFixtureAccount] = m
	if err = Save(root, r); err != nil {
		t.Fatal(err)
	}
	var out any
	if err = host.callUntil("maintenance.inspect", map[string]any{"account": maintenanceFixtureAccount, "deadline_ms": deadline.UnixMilli()}, &out, deadline); err == nil {
		t.Fatal("disabled account retained authority")
	}
	if _, err = os.Stat(filepath.Join(root, "generated")); !os.IsNotExist(err) {
		t.Fatal("invalid operation generated")
	}
}

func TestMaintenanceDisabledDoesNotMaterializeHost(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	t.Setenv("PATH", t.TempDir())
	value, err := MaintenanceRun(context.Background(), root, "")
	if err != nil || value.Status != "disabled" {
		t.Fatal(value, err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("disabled command created a host or state")
	}
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), "token") {
		t.Fatal("unexpected credential field")
	}
}

func TestMaintenanceObserverHonorsCurrentDesktopConfig(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "extensions")
	inbox := filepath.Join(base, "desktop", "inbox")
	if err := os.MkdirAll(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base, root, inbox} {
		if err := privatefile.Protect(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(base, "desktop.json")
	observed := time.Now().UTC()
	reset := observed.Add(time.Hour)
	snap := maintenance.Snapshot{Account: maintenanceFixtureAccount, Enabled: true, Fresh: true, ObservedAt: observed, UsageKnown: true,
		Windows: []maintenance.Window{{Name: "secondary", Used: 12, SpanSeconds: 604800, ResetsAt: &reset}}}
	observer := maintenanceObserver(root, "")
	count := func() int {
		entries, err := os.ReadDir(inbox)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				n++
			}
		}
		return n
	}
	if err := observer(snap); err != nil || count() != 0 {
		t.Fatal("missing settings enabled a notification", err)
	}
	write := func(enabled bool, threshold uint32) {
		raw, _ := json.Marshal(map[string]any{"enabled": enabled, "usage_alert": threshold, "inbox": inbox})
		if err := Atomic(cfg, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(false, 80)
	if err := maintenanceObserver(root, inbox)(snap); err != nil || count() != 0 {
		t.Fatal("inbox argument bypassed disabled state", err)
	}
	write(true, 0)
	if err := observer(snap); err != nil || count() != 0 {
		t.Fatal("zero thresholds enabled a quota notification", err)
	}
	write(true, 80)
	if err := maintenanceObserver(root, filepath.Join(base, "other"))(snap); err != desktop.ErrConfig || count() != 0 {
		t.Fatal("inbox argument redirected an event", err)
	}
	if err := observer(snap); err != nil || count() != 1 {
		t.Fatal("fresh observation was not published to configured inbox", err)
	}
	write(false, 80)
	if err := observer(snap); err != nil || count() != 1 {
		t.Fatal("observer did not re-read disable", err)
	}
	if err := Atomic(cfg, []byte(`{"enabled":true,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := observer(snap); err == nil || count() != 1 {
		t.Fatal("invalid settings published an event", err)
	}
}

func TestMaintenanceDesktopFailureDoesNotFailCompletedAction(t *testing.T) {
	root, _ := maintenanceHostFixture(t, "success")
	config := filepath.Join(filepath.Dir(root), "desktop.json")
	if err := Atomic(config, []byte(`{"enabled":true,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := MaintenanceRun(t.Context(), root, "")
	if err != nil || len(result.Results) != 1 || result.Results[0].Status != "warmup_completed" {
		t.Fatal("notification config changed primary result", result, err)
	}
	generated, _ := os.ReadFile(filepath.Join(root, "generated"))
	if string(generated) != "1" {
		t.Fatal("expected exactly one synthetic action")
	}
}
