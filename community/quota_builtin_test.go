package community

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

const fixtureCodexKey = "builtin:codex#11111111111111111111"

func nativeQuotaJWT(account string, expires int64, user string) string {
	data, _ := json.Marshal(map[string]any{"exp": expires, "sub": user, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	return "fixture." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
}
func nativeQuotaFixture(t *testing.T) (string, Registry, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(root, 0700); err != nil {
		t.Fatal(err)
	}
	scripts, err := BuiltinScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(scripts["codex.mjs"])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	r := Registry{Version: 1, Plugins: []Entry{{Name: "builtin-codex", Source: "builtin-codex", Kind: "community", Enabled: true,
		Path: scripts["codex.mjs"], SHA256: hex.EncodeToString(digest[:]), Version: "0.1.0"}},
		Accounts: map[string]AccountMeta{fixtureCodexKey: {Enabled: true, Label: "PRIVATE-LABEL"}},
		Prefer:   map[string]string{"builtin:codex": "builtin-codex"}}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "native-auth.json")
	value := map[string]any{"tokens": map[string]any{"access_token": nativeQuotaJWT("fixture-account", time.Now().Unix()+3600, "fixture-user"),
		"refresh_token": "PRIVATE-NATIVE-REFRESH", "account_id": "fixture-account"}}
	raw, _ := json.Marshal(value)
	if err := Atomic(native, raw, 0600); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{fixtureCodexKey: map[string]any{"type": "oauth", "accountId": "fixture-account", "refreshOwner": "external",
		"sourceAuthFile": native, "access": "PRIVATE-STAGED-ACCESS", "refresh": "PRIVATE-STAGED-REFRESH", "expires": 1}}
	raw, _ = json.Marshal(auth)
	if err := Atomic(filepath.Join(root, "plugin-auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root, r, native
}
func mockNativeQuota(t *testing.T, root, mode string) string {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun is optional; native quota fixture requires Bun")
	}
	calls := filepath.Join(root, "calls.jsonl")
	script := filepath.Join(root, "network-preload.mjs")
	mock := `import {appendFileSync,readFileSync,writeFileSync} from 'node:fs';
const root=process.env.ZCALLD_QUOTA_FIXTURE_ROOT, mode=process.env.ZCALLD_QUOTA_FIXTURE_MODE;
globalThis.__zcalldQuotaFixture=true;
globalThis.fetch=async(input,init={})=>{
const record=(kind)=>appendFileSync(root+'/calls.jsonl',JSON.stringify({kind})+'\n');
const request=new Request(input,init);
// Bun currently exposes include on a reconstructed Request even when init.credentials is omit. Check the actual fetch argument.
if(request.url!=='https://chatgpt.com/backend-api/wham/usage'||request.method!=='GET'||request.redirect!=='error'||init.credentials!=='omit') {record('forbidden');throw new Error('network forbidden');}
const native=JSON.parse(readFileSync(root+'/native-auth.json','utf8'));
if(request.headers.get('authorization')!=='Bearer '+native.tokens.access_token){record('wrong-auth');throw new Error('wrong synthetic credential');}
record('GET');
if(mode==='wait'){await new Promise(resolve=>{const stop=()=>{record('abort');resolve()};if(request.signal.aborted)stop();else request.signal.addEventListener('abort',stop,{once:true})});throw new DOMException('cancelled','AbortError');}
if(mode==='changed'){const parts=native.tokens.access_token.split('.');const claims=JSON.parse(Buffer.from(parts[1],'base64url'));claims.sub='other-user';native.tokens.access_token=parts[0]+'.'+Buffer.from(JSON.stringify(claims)).toString('base64url')+'.'+parts[2];writeFileSync(root+'/native-auth.json',JSON.stringify(native));}
if(mode==='redirect')return new Response('',{status:302,headers:{location:'https://never.invalid/'}});
const usage={rate_limit:{allowed:true,limit_reached:false,primary_window:{used_percent:37.5,limit_window_seconds:18000,reset_at:Math.floor(Date.now()/1000)+3600},secondary_window:{used_percent:12,limit_window_seconds:604800,reset_at:Math.floor(Date.now()/1000)+500000}},credits:{balance:'PRIVATE'},account:'PRIVATE'};
const body=mode==='large'?JSON.stringify({...usage,padding:'x'.repeat(65536)}):mode==='malformed'?JSON.stringify({rate_limit:{primary_window:{used_percent:'unknown'}}}):JSON.stringify(usage);
return new Response(body,{headers:{'content-type':'application/json'}});
};`
	if err := Atomic(script, []byte(mock), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUN_OPTIONS", "--preload="+script)
	t.Setenv("ZCALLD_QUOTA_FIXTURE_ROOT", root)
	t.Setenv("ZCALLD_QUOTA_FIXTURE_MODE", mode)
	// Never start a host until the exact Bun command environment has proved its
	// closed mock fetch is loaded. No fixture falls back to real network access.
	cmd := exec.Command(bun, "-e", "console.log(globalThis.__zcalldQuotaFixture===true?'PRELOAD_OK':'PRELOAD_MISSING')")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "PRELOAD_OK" {
		t.Fatalf("network guard unavailable: %v", err)
	}
	return calls
}
func nativeCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var v struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, v.Kind)
	}
	return kinds
}

func TestBuiltinQuotaCapabilityAttestsWholeBundleWithoutHost(t *testing.T) {
	for _, change := range []string{"valid", "disabled", "edited-import", "forged-name", "bad-sha", "extra-gateway"} {
		t.Run(change, func(t *testing.T) {
			root, r, _ := nativeQuotaFixture(t)
			switch change {
			case "disabled":
				r.Plugins[0].Enabled = false
			case "edited-import":
				p := filepath.Join(filepath.Dir(r.Plugins[0].Path), "common.mjs")
				if err := os.WriteFile(p, []byte("custom code"), 0600); err != nil {
					t.Fatal(err)
				}
			case "forged-name":
				r.Plugins[0].Path = filepath.Join(root, "custom.mjs")
				if err := Atomic(r.Plugins[0].Path, []byte("throw new Error('must not run')"), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad-sha":
				r.Plugins[0].SHA256 = strings.Repeat("0", 64)
			case "extra-gateway":
				r.Plugins[0].Gateway = filepath.Join(root, "custom.mjs")
			}
			if err := Save(root, r); err != nil {
				t.Fatal(err)
			}
			auth, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			registry, _ := os.ReadFile(filepath.Join(root, "registry.json"))
			t.Setenv("PATH", t.TempDir())
			value, err := QuotaList(root)
			if err != nil {
				t.Fatal(err)
			}
			account := value["accounts"].([]any)[0].(map[string]any)
			if account["can_refresh"] != (change == "valid") {
				t.Fatal(change, account)
			}
			after, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			afterRegistry, _ := os.ReadFile(filepath.Join(root, "registry.json"))
			if string(after) != string(auth) || string(afterRegistry) != string(registry) {
				t.Fatal("local listing changed private state")
			}
		})
	}
}

func TestBuiltinQuotaActualHostReadsNativeOwnerWithoutRefresh(t *testing.T) {
	for _, mode := range []string{"ok", "expired", "wrong-account", "redirect", "large", "malformed", "changed", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			root, r, native := nativeQuotaFixture(t)
			if mode == "disabled" {
				r.Accounts[fixtureCodexKey] = AccountMeta{Enabled: false}
				if err := Save(root, r); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired" || mode == "wrong-account" {
				account := "fixture-account"
				expires := time.Now().Unix() + 3600
				if mode == "expired" {
					expires = time.Now().Unix() - 1
				} else {
					account = "other-account"
				}
				raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": nativeQuotaJWT(account, expires, "fixture-user"), "account_id": account, "refresh_token": "PRIVATE-NATIVE-REFRESH"}})
				if err := Atomic(native, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := mockNativeQuota(t, root, mode)
			authBefore, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			nativeBefore, _ := os.ReadFile(native)
			value, err := QuotaRefresh(root, fixtureCodexKey)
			if err != nil || value["ok"] != (mode == "ok") {
				t.Fatal(mode, value, err)
			}
			got := nativeCalls(t, calls)
			expected := 1
			if mode == "expired" || mode == "wrong-account" || mode == "disabled" {
				expected = 0
			}
			if len(got) != expected || (expected == 1 && got[0] != "GET") {
				t.Fatal("unexpected outbound calls", got)
			}
			authAfter, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			nativeAfter, _ := os.ReadFile(native)
			if string(authBefore) != string(authAfter) || (mode != "changed" && string(nativeBefore) != string(nativeAfter)) {
				t.Fatal("usage renewed or wrote auth")
			}
			if mode == "ok" {
				cache, err := readQuotaCache(root)
				if err != nil {
					t.Fatal(err)
				}
				windows := cache.Accounts[fixtureCodexKey].Windows
				if len(windows) != 2 || windows[0].Name != "primary" || windows[0].SpanSeconds != 18000 || *windows[0].Used != 37.5 {
					t.Fatal(windows)
				}
			} else if _, err := os.Stat(filepath.Join(root, quotaCacheFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed quota read committed")
			}
		})
	}
}

func TestBuiltinQuotaRejectsScopeMutationAndCancelsGET(t *testing.T) {
	root, r, _ := nativeQuotaFixture(t)
	calls := mockNativeQuota(t, root, "wait")
	host, err := startBuiltinCodexHost(root, time.Now().Add(3*time.Second), time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	var invalid map[string]any
	if err := host.callUntil("codex.usage", map[string]any{"account": fixtureCodexKey, "deadline_ms": time.Now().Add(time.Second).UnixMilli(), "url": "https://never.invalid/"}, &invalid, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if invalid["ok"] != false || len(nativeCalls(t, calls)) != 0 {
		t.Fatal("caller URL accepted")
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	err = host.callUntil("codex.usage", map[string]any{"account": fixtureCodexKey, "deadline_ms": deadline.UnixMilli()}, &invalid, deadline)
	host.Close()
	if err != nil && !errors.Is(err, errHostDeadline) {
		t.Fatal(err)
	}
	got := nativeCalls(t, calls)
	if strings.Join(got, ",") != "GET,abort" {
		t.Fatal("pending usage not cancelled exactly once", got)
	}
	// The next host must check actual account state, not trust caller strings.
	r.Accounts[fixtureCodexKey] = AccountMeta{Enabled: false}
	if err := Save(root, r); err != nil {
		t.Fatal(err)
	}
	value, err := QuotaRefresh(root, fixtureCodexKey)
	if err != nil || value["error"] != "account_disabled" || len(nativeCalls(t, calls)) != 2 {
		t.Fatal(value, err)
	}
}

func TestBuiltinQuotaNotificationUsesSharedCanonicalIdentity(t *testing.T) {
	reading := quotaReading{At: time.Now().UTC().Format(time.RFC3339Nano), codex: true}
	primary, secondary := 88.0, 20.0
	reading.Windows = []quotaWindow{{Name: "primary", Used: &primary, SpanSeconds: 18000}, {Name: "secondary", Used: &secondary, SpanSeconds: 604800}}
	event, ok := quotaDesktopObservation(fixtureCodexKey, reading, time.Now())
	if !ok || event.SourceID != quotaDigest("zcalld:codex-account:v1\x00"+fixtureCodexKey) || len(event.Windows) != 2 {
		t.Fatal(event)
	}
	if event.Windows[0].ID != "primary" || event.Windows[1].ID != "secondary" || event.Windows[0].Pool != "account" || event.Windows[0].SpanSeconds != 18000 || event.Balance != nil {
		t.Fatal(event)
	}
}
