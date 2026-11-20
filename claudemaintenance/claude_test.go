package claudemaintenance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

const syntheticUser = "synthetic@example.invalid"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "-p" || os.Args[1] == "--capabilities" || os.Args[1] == "--claude-binary") {
		fakeCLI()
		return
	}
	os.Exit(m.Run())
}

func fakeCLI() {
	args := os.Args[1:]
	if args[0] == "--capabilities" {
		fmt.Printf(`{"schema_version":1,"source_revision":%q,"interactive":true,"stream_json":true,"print_fallback":false,"claude_binary_absolute":true,"config_dir_explicit":true,"profile_path_explicit":true,"wsl":false}`, referenceRevision)
		return
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		configDir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	profile := filepath.Join(configDir, ".claude.json")
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		profile = filepath.Join(filepath.Dir(configDir), ".claude.json")
	}
	stream := args[0] == "--claude-binary"
	if stream {
		for i, a := range args {
			if a == "--config-dir" && i+1 < len(args) {
				configDir = args[i+1]
			}
			if a == "--profile" && i+1 < len(args) {
				profile = args[i+1]
			}
		}
	}
	var state map[string]any
	b, _ := os.ReadFile(filepath.Join(configDir, "fake.json"))
	_ = json.Unmarshal(b, &state)
	if slices.Contains(args, "/usage") {
		if state["switch"] == true {
			p := map[string]any{"oauthAccount": identity{Email: syntheticUser, Organization: "changed-org", Account: "fake-member"}}
			b, _ := json.Marshal(p)
			_ = os.WriteFile(profile, b, 0600)
		}
		if state["usage_error"] == true {
			fmt.Print(`[{"type":"result","is_error":true,"result":"unavailable"}]`)
			return
		}
		used := 0.0
		if value, ok := state["used"].(float64); ok {
			used = value
		}
		reset := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
		fmt.Printf(`[{"usage_report":{"rate_limits":{"limits":[{"kind":"weekly_all","percent":%v,"resets_at":%q}],"extra_usage":{"is_enabled":true,"monthly_limit":10000,"used_credits":5000}}}},{"type":"result","is_error":false,"result":"synthetic usage"}]`, used, reset)
		return
	}
	input := []byte{}
	if stream {
		if state["switch_ready"] == true {
			p := map[string]any{"oauthAccount": identity{Email: syntheticUser, Organization: "changed-org", Account: "fake-member"}}
			b, _ := json.Marshal(p)
			_ = os.WriteFile(profile, b, 0600)
		}
		fmt.Println(`{"type":"system","subtype":"init"}`)
		var raw json.RawMessage
		decoder := json.NewDecoder(os.Stdin)
		if decoder.Decode(&raw) != nil {
			os.Exit(81)
		}
		input = raw
	} else {
		input, _ = io.ReadAll(io.LimitReader(os.Stdin, 4096))
	}
	line, _ := json.Marshal(map[string]any{"args": args, "input": string(input), "home": os.Getenv("HOME"), "config": os.Getenv("CLAUDE_CONFIG_DIR"), "forbidden": os.Getenv("ANTHROPIC_API_KEY") + os.Getenv("ANTHROPIC_BASE_URL") + os.Getenv("NODE_OPTIONS"), "pid": os.Getpid()})
	f, e := os.OpenFile(filepath.Join(configDir, "generations.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(82)
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	if state["uncertain"] == true {
		os.Exit(83)
	}
	if state["hang"] == true {
		time.Sleep(time.Minute)
		return
	}
	fmt.Println(`{"type":"result","is_error":false,"result":"hi"}`)
	if stream {
		_, _ = io.Copy(io.Discard, os.Stdin)
	} // EOF must follow result, not input.
}

type fixture struct {
	root, source, bindings, config, profile string
	bindingsValue                           Bindings
}

func protectWrite(t *testing.T, path string, v any) {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = privatefile.Protect(path, 0600); e != nil {
		t.Fatal(e)
	}
}
func privateDir(t *testing.T, path string) {
	t.Helper()
	if e := os.MkdirAll(path, 0700); e != nil {
		t.Fatal(e)
	}
	if e := privatefile.Protect(path, 0700); e != nil {
		t.Fatal(e)
	}
}
func newFixture(t *testing.T, settings map[string]any, native bool) fixture {
	t.Helper()
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	privateDir(t, base)
	f := fixture{root: filepath.Join(base, "extensions"), source: filepath.Join(base, "snapshot"), bindings: filepath.Join(base, "bindings.json"), config: filepath.Join(base, "account")}
	privateDir(t, f.root)
	privateDir(t, f.source)
	privateDir(t, f.config)
	f.profile = filepath.Join(f.config, ".claude.json")
	if native {
		home := filepath.Join(base, "native-home")
		privateDir(t, home)
		f.config = filepath.Join(home, ".claude")
		privateDir(t, f.config)
		f.profile = filepath.Join(home, ".claude.json")
	}
	identity := identity{Email: syntheticUser, Organization: "fake-org", Account: "fake-member"}
	protectWrite(t, f.profile, map[string]any{"oauthAccount": identity, "unrelated": "retained"})
	protectWrite(t, filepath.Join(f.source, "settings.json"), settings)
	protectWrite(t, filepath.Join(f.source, "logins.json"), []map[string]any{{"id": "login-id", "agent": "claude", "user": syntheticUser, "profile": identity, "on": true, "auth": map[string]string{"secret": "SYNTHETIC-NEVER-IMPORT"}}})
	protectWrite(t, filepath.Join(f.source, "providers.json"), map[string]any{"providers": []map[string]any{{"id": "claude"}}})
	protectWrite(t, filepath.Join(f.source, "claude-warmup.json"), map[string]any{})
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	self, e = filepath.EvalSymlinks(self)
	if e != nil {
		t.Fatal(e)
	}
	owner := "saved"
	if native {
		owner = "current"
	}
	f.bindingsValue = Bindings{Version: 1, Binary: self, Helper: self, Accounts: []Binding{{LoginID: "login-id", UserHash: digest([]byte(syntheticUser)), ConfigDir: f.config, Profile: f.profile, Ownership: owner, Enabled: true}}}
	protectWrite(t, f.bindings, f.bindingsValue)
	return f
}
func (f fixture) apply(t *testing.T) string {
	t.Helper()
	p, e := Plan(t.Context(), f.root, f.source, f.bindings, "", false)
	if e != nil {
		t.Fatal(e)
	}
	r, e := Plan(t.Context(), f.root, f.source, f.bindings, p.PlanHash, true)
	if e != nil {
		t.Fatal(e)
	}
	if r.Enabled || r.CredentialsImported || r.SchedulerChanged || !r.Applied || len(r.AccountIDs) != 1 {
		t.Fatal(r)
	}
	return r.AccountIDs[0]
}
func fakeState(t *testing.T, f fixture, v map[string]any) {
	protectWrite(t, filepath.Join(f.config, "fake.json"), v)
}
func generations(t *testing.T, f fixture) []map[string]any {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(f.config, "generations.jsonl"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if e = json.Unmarshal([]byte(line), &m); e != nil {
			t.Fatal(e)
		}
		out = append(out, m)
	}
	return out
}

func TestPlanPreservesModesClocksHistoryAndDoesNotImportCredentials(t *testing.T) {
	for _, mode := range []string{"off", "week", "all"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, map[string]any{"claudeWarmup": mode, "claudeWarmAt": "05:00", "claudeWarmAts": []string{"14:05:30", "06:00", "14:05"}, "codexAutoReset": []string{syntheticUser}}, false)
			protectWrite(t, filepath.Join(f.source, "claude-warmup.json"), map[string]any{syntheticUser: map[string]any{"7 days": map[string]any{"used": 7, "pending": true, "daily": "2026-11-19 14:05", "idle": 2}}})
			key := f.apply(t)
			d, e := Store(f.root).Read()
			if e != nil {
				t.Fatal(e)
			}
			p := d.Policy.Accounts[key]
			if p.WeeklyWarmup != (mode != "off") || p.FiveHourWarmup != (mode == "all") || !slices.Equal(p.WarmAts, []string{"06:00", "14:05"}) || p.WeeklyReset || p.ExpiringReset {
				t.Fatal(p)
			}
			if d.Ledger[key].Warm == nil || d.Ledger[key].Windows["7 days"].Daily != "2026-11-19 14:05" {
				t.Fatal("pending/history lost")
			}
			b, _ := os.ReadFile(filepath.Join(Store(f.root).Directory, "state.json"))
			if bytesContain(b, "SYNTHETIC-NEVER-IMPORT") || bytesContain(d.Adapter, syntheticUser) {
				t.Fatal("credential or raw profile imported")
			}
			if _, e = os.Stat(filepath.Join(f.config, "generations.jsonl")); !os.IsNotExist(e) {
				t.Fatal("plan ran CLI")
			}
		})
	}
}
func bytesContain(b []byte, s string) bool { return strings.Contains(string(b), s) }

func TestExplicitEmptyClockAndOffStillPreservesPendingLedger(t *testing.T) {
	f := newFixture(t, map[string]any{"claudeWarmup": "off", "claudeWarmAt": "06:00", "claudeWarmAts": []string{}}, false)
	protectWrite(t, filepath.Join(f.source, "claude-warmup.json"), map[string]any{syntheticUser: map[string]any{"7 days": map[string]any{"used": 0, "pending": true}}})
	key := f.apply(t)
	d, _ := Store(f.root).Read()
	if len(d.Policy.Accounts[key].WarmAts) != 0 || d.Ledger[key].Warm == nil {
		t.Fatal("off import discarded history")
	}
	r, e := Manual(t.Context(), f.root, key, false)
	if e != nil || r.Results[0].Status != "pending_review" || len(generations(t, f)) != 0 {
		t.Fatal(r, e)
	}
}

func TestPlanIdentityAndSourceAreBound(t *testing.T) {
	for _, change := range []string{"member", "organization", "source", "binding"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, map[string]any{"claudeWarmup": "week"}, false)
			p, e := Plan(t.Context(), f.root, f.source, f.bindings, "", false)
			if e != nil {
				t.Fatal(e)
			}
			switch change {
			case "member", "organization":
				i := identity{Email: syntheticUser, Organization: "fake-org", Account: "fake-member"}
				if change == "member" {
					i.Account = "other"
				} else {
					i.Organization = "other"
				}
				protectWrite(t, f.profile, map[string]any{"oauthAccount": i})
			case "source":
				protectWrite(t, filepath.Join(f.source, "settings.json"), map[string]any{"claudeWarmup": "all"})
			case "binding":
				f.bindingsValue.TrustWorkspace = true
				protectWrite(t, f.bindings, f.bindingsValue)
			}
			if _, e = Plan(t.Context(), f.root, f.source, f.bindings, p.PlanHash, true); e == nil {
				t.Fatal("changed input accepted")
			}
			if _, e = os.Stat(Store(f.root).Directory); !os.IsNotExist(e) {
				t.Fatal("rejected plan changed state")
			}
		})
	}
}

func TestSavedOwnerAndDuplicateBindingRejected(t *testing.T) {
	f := newFixture(t, map[string]any{}, false)
	b, _ := os.ReadFile(filepath.Join(f.source, "logins.json"))
	var logins []map[string]any
	_ = json.Unmarshal(b, &logins)
	logins[0]["held"] = true
	protectWrite(t, filepath.Join(f.source, "logins.json"), logins)
	if _, e := Plan(t.Context(), f.root, f.source, f.bindings, "", false); e == nil {
		t.Fatal("saved held owner admitted")
	}
	logins[0]["held"] = false
	protectWrite(t, filepath.Join(f.source, "logins.json"), logins)
	f.bindingsValue.Accounts = append(f.bindingsValue.Accounts, f.bindingsValue.Accounts[0])
	protectWrite(t, f.bindings, f.bindingsValue)
	if _, e := Plan(t.Context(), f.root, f.source, f.bindings, "", false); e == nil {
		t.Fatal("duplicate account admitted")
	}
}

func TestDisabledClaudeCreatesNoStateOrProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	r, e := Run(t.Context(), root)
	if e != nil || r.Status != "disabled" {
		t.Fatal(r, e)
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		t.Fatal("disabled created state")
	}
}

func TestRealFakeCLIPrintAndInteractiveExplicitLayouts(t *testing.T) {
	for _, printMode := range []bool{false, true} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("print=%v/native=%v", printMode, native), func(t *testing.T) {
				f := newFixture(t, map[string]any{"claudePrintMode": printMode}, native)
				key := f.apply(t)
				t.Setenv("ANTHROPIC_API_KEY", "FORBIDDEN")
				t.Setenv("ANTHROPIC_BASE_URL", "https://forbidden.invalid")
				t.Setenv("NODE_OPTIONS", "FORBIDDEN")
				before, _ := os.ReadFile(f.profile)
				r, e := Manual(t.Context(), f.root, key, false)
				if e != nil || len(r.Results) != 1 || r.Results[0].Status != "completed" {
					t.Fatal(r, e)
				}
				g := generations(t, f)
				if len(g) != 1 || g[0]["forbidden"] != "" {
					t.Fatal(g)
				}
				if printMode && g[0]["input"] != "hi\n" {
					t.Fatal("prompt drift")
				}
				if !printMode && !strings.Contains(g[0]["input"].(string), `"text":"hi"`) {
					t.Fatal("interactive prompt drift")
				}
				if native && g[0]["home"] != filepath.Dir(f.profile) {
					t.Fatal("native HOME changed")
				}
				after, _ := os.ReadFile(f.profile)
				if string(before) != string(after) {
					t.Fatal("profile modified")
				}
				d, _ := Store(f.root).Read()
				if d.Policy.Enabled || d.Ledger[key].Warm != nil {
					t.Fatal("manual enabled scheduler or retained completed intent")
				}
				marks, _ := filepath.Glob(f.config + ".run-*.lock")
				if len(marks) != 0 {
					t.Fatal("run mark leaked")
				}
			})
		}
	}
}

func TestNoExtraCapAndIdentityChangeNeverGenerate(t *testing.T) {
	for _, kind := range []string{"no_extra", "cap", "unknown_usage", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, map[string]any{"claudePrintMode": true, "claudeNoExtraUsage": kind == "no_extra"}, false)
			if kind == "cap" {
				protectWrite(t, filepath.Join(f.source, "providers.json"), map[string]any{"providers": []map[string]any{{"id": "claude", "accountWindowAmounts": map[string]any{syntheticUser: map[string]int{"usage credits": 40}}}}})
			}
			state := map[string]any{"used": 100}
			if kind == "unknown_usage" {
				state["usage_error"] = true
			}
			if kind == "identity" {
				state["switch"] = true
			}
			fakeState(t, f, state)
			key := f.apply(t)
			r, e := Manual(t.Context(), f.root, key, false)
			if e != nil || len(r.Results) != 1 || r.Results[0].Status != "quota_or_model_unavailable" || len(generations(t, f)) != 0 {
				t.Fatal(r, e)
			}
		})
	}
}

func TestUnknownWarmupIsNotReplayed(t *testing.T) {
	f := newFixture(t, map[string]any{"claudePrintMode": true}, false)
	key := f.apply(t)
	fakeState(t, f, map[string]any{"uncertain": true})
	first, e := Manual(t.Context(), f.root, key, false)
	if e != nil || first.Results[0].Status != "unknown" {
		t.Fatal(first, e)
	}
	fakeState(t, f, map[string]any{})
	second, e := Manual(t.Context(), f.root, key, false)
	if e != nil || second.Results[0].Status != "pending_review" || len(generations(t, f)) != 1 {
		t.Fatal(second, e)
	}
}

func TestUsageRowsTextUnknownAndNoMeterPlan(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		raw   string
		count int
		ok    bool
	}{
		{`[{"usage_report":{"rate_limits":{"limits":[]}}},{"type":"result","result":"no meters"}]`, 0, true},
		{`[{"usage_report":{"rate_limits":{"five_hour":null,"seven_day":null}}},{"type":"result","result":"usage unavailable"}]`, 0, false},
		{`[{"usage_report":{"rate_limits":{"five_hour":null,"seven_day":null}}},{"type":"result","result":"Current session: 0% used"}]`, 1, true},
		{`[{"usage_report":{"rate_limits":{"five_hour":{"utilization":20},"seven_day":null}}},{"type":"result","result":"Current session: 20% used"}]`, 1, true},
		{`{"type":"result","result":"Current session: 20% used · resets 12pm (UTC)\nCurrent week (all models): 2% used · resets Nov 25 at 10am (UTC)"}`, 2, true},
		{`{"type":"result","result":"usage unavailable"}`, 0, false},
		{`[{"usage_report":{"rate_limits":{"limits":[{"kind":"weekly_all","percent":101}]}}},{"type":"result","result":""}]`, 0, false},
		{`[{"usage_report":{"rate_limits":{"limits":[{"kind":"weekly_all","percent":2},{"kind":"weekly_all","percent":3}]}}},{"type":"result","result":""}]`, 0, false},
	}
	for i, c := range cases {
		u, e := parseUsage([]byte(c.raw), now)
		if (e == nil) != c.ok || (c.ok && len(u.Windows) != c.count) {
			t.Fatalf("case %d: %#v %v", i, u, e)
		}
	}
}

func TestInteractiveReadyRechecksIdentityBeforeHi(t *testing.T) {
	f := newFixture(t, map[string]any{}, false)
	key := f.apply(t)
	fakeState(t, f, map[string]any{"switch_ready": true})
	r, e := Manual(t.Context(), f.root, key, false)
	if e != nil || len(r.Results) != 1 || r.Results[0].Status != "unknown" || len(generations(t, f)) != 0 {
		t.Fatal(r, e)
	}
	d, _ := Store(f.root).Read()
	if d.Ledger[key].Warm == nil {
		t.Fatal("started interaction must remain uncertain")
	}
}

func TestClaudeCannotAcquireResetCardPolicy(t *testing.T) {
	f := newFixture(t, map[string]any{}, false)
	key := f.apply(t)
	d, _ := Store(f.root).Read()
	d.Policy.Accounts[key] = maintenance.AccountPolicy{WeeklyReset: true, ResetLeadMinutes: 30}
	protectWrite(t, filepath.Join(Store(f.root).Directory, "state.json"), d)
	if _, e := Store(f.root).Read(); e == nil {
		t.Fatal("Claude reset policy accepted")
	}
	r, e := (Host{}).Consume(context.Background(), key, maintenance.ConsumeRequest{})
	if e == nil || r.Status != "not_sent" {
		t.Fatal(r, e)
	}
}
