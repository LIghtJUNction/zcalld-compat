package community

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func quotaFixture(t *testing.T) (string, Registry) {
	t.Helper()
	root := t.TempDir()
	r := Registry{Version: 1, Plugins: []Entry{{Name: "quota", Kind: "community", Source: "npm:quota-fixture@1.0.0", Path: filepath.Join(root, "fixture.mjs"), SHA256: "fixed", Enabled: true}},
		Accounts: map[string]AccountMeta{"quota#one": {Label: "One", Enabled: true}, "quota#two": {Label: "Two", Enabled: true}}}
	if e := Save(root, r); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{
		"plugin-auth.json":      `{"quota#one":{"type":"oauth","access":"PRIVATE-ONE","expires":4102444800000},"quota#two":{"type":"api","key":"PRIVATE-TWO"}}`,
		"plugin-providers.json": `[{"id":"quota","spec":"quota-fixture","usage":true,"apiKey":"NEVER-EXPOSE"}]`,
	} {
		if e := Atomic(filepath.Join(root, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return root, r
}
func reading(used float64) quotaReading {
	return quotaReading{At: "2026-11-20T12:00:00Z", Windows: []quotaWindow{{Name: "day", Used: &used}}}
}
func TestQuotaListIsLocalAndLeavesPrivateStateUntouched(t *testing.T) {
	root, _ := quotaFixture(t)
	auth, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	registry, _ := os.ReadFile(filepath.Join(root, "registry.json"))
	t.Setenv("PATH", t.TempDir())
	value, e := QuotaList(root)
	if e != nil {
		t.Fatal(e)
	}
	if value["local_only"] != true || len(value["accounts"].([]any)) != 2 {
		t.Fatal(value)
	}
	first := value["accounts"].([]any)[0].(map[string]any)
	if first["can_refresh"] != true {
		t.Fatal(first)
	}
	b, _ := json.Marshal(value)
	if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "NEVER-EXPOSE") {
		t.Fatal("private fields escaped")
	}
	if _, e = os.Stat(filepath.Join(root, "runtime")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("local list started/materialized a host")
	}
	for name, original := range map[string][]byte{"plugin-auth.json": auth, "registry.json": registry} {
		b, _ := os.ReadFile(filepath.Join(root, name))
		if string(b) != string(original) {
			t.Fatal("list changed", name)
		}
	}
}
func TestQuotaRejectsDisabledUnsupportedAndExternalBeforeHost(t *testing.T) {
	for _, which := range []string{"account_disabled", "plugin_disabled", "unsupported", "external_owner", "capability_unknown"} {
		t.Run(which, func(t *testing.T) {
			root, r := quotaFixture(t)
			switch which {
			case "account_disabled":
				r.Accounts["quota#one"] = AccountMeta{Enabled: false}
				_ = Save(root, r)
			case "plugin_disabled":
				r.Plugins[0].Enabled = false
				_ = Save(root, r)
			case "unsupported":
				_ = Atomic(filepath.Join(root, "plugin-providers.json"), []byte(`[{"id":"quota","spec":"npm:quota-fixture","usage":false}]`), 0600)
			case "capability_unknown":
				_ = os.Remove(filepath.Join(root, "plugin-providers.json"))
			case "external_owner":
				_ = Atomic(filepath.Join(root, "plugin-auth.json"), []byte(`{"quota#one":{"type":"oauth","access":"EXTERNAL-PRIVATE","refreshOwner":"external","expires":1}}`), 0600)
			}
			before, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			value, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) {
				t.Fatal("host invoked for blocked account")
				return quotaReading{}, nil
			})
			if e != nil || value["ok"] != false || value["error"] != which {
				t.Fatal(value, e)
			}
			after, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
			if string(before) != string(after) {
				t.Fatal("auth changed")
			}
		})
	}
}
func TestQuotaSuccessFailureAndConcurrentCacheMerge(t *testing.T) {
	root, _ := quotaFixture(t)
	oldQuota := filepath.Join(root, "quotas.json")
	_ = os.WriteFile(oldQuota, []byte("legacy cache must stay"), 0600)
	var wg sync.WaitGroup
	for _, id := range []string{"quota#one", "quota#two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			v, e := quotaRefresh(root, id, func(string, Entry, string, string) (quotaReading, error) {
				time.Sleep(10 * time.Millisecond)
				return reading(25), nil
			})
			if e != nil || v["ok"] != true {
				t.Error(v, e)
			}
		}(id)
	}
	wg.Wait()
	cache, e := readQuotaCache(root)
	if e != nil || len(cache.Accounts) != 2 {
		t.Fatal(cache, e)
	}
	before, _ := os.ReadFile(filepath.Join(root, quotaCacheFile))
	value, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) {
		return quotaReading{}, errors.New("SECRET https://private.invalid/?token=PRIVATE")
	})
	if e != nil || value["error"] != "provider_error" {
		t.Fatal(value, e)
	}
	after, _ := os.ReadFile(filepath.Join(root, quotaCacheFile))
	if string(before) != string(after) {
		t.Fatal("failed update erased prior reading")
	}
	b, _ := json.Marshal(value)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "private.invalid") {
		t.Fatal("raw error escaped")
	}
	original, _ := os.ReadFile(oldQuota)
	if string(original) != "legacy cache must stay" {
		t.Fatal("legacy quotas changed")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(root, quotaCacheFile))
		if info.Mode().Perm() != 0600 {
			t.Fatal("cache is not private")
		}
	}
}
func TestQuotaRechecksDisabledAccountBeforeCacheCommit(t *testing.T) {
	root, r := quotaFixture(t)
	v, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) {
		r.Accounts["quota#one"] = AccountMeta{Enabled: false}
		if e := Save(root, r); e != nil {
			t.Fatal(e)
		}
		return reading(10), nil
	})
	if e != nil || v["error"] != "account_or_plugin_changed" {
		t.Fatal(v, e)
	}
	if _, e = os.Stat(filepath.Join(root, quotaCacheFile)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("stale response committed")
	}
}
func TestQuotaCachedCapabilityShapesAndAmbiguousPackages(t *testing.T) {
	root, r := quotaFixture(t)
	for _, data := range []string{`{"providers":[{"id":"quota","spec":"npm:quota-fixture","usage":true}]}`, `{"quota":{"spec":"npm:quota-fixture","usage":true}}`} {
		_ = Atomic(filepath.Join(root, "plugin-providers.json"), []byte(data), 0600)
		if _, reason := quotaEntry(r, quotaCapabilities(root)["quota"]); reason != "" {
			t.Fatal(reason)
		}
	}
	r.Plugins = append(r.Plugins, r.Plugins[0])
	r.Plugins[1].Name = "other"
	if _, reason := quotaEntry(r, quotaCapabilities(root)["quota"]); reason != "plugin_unavailable" {
		t.Fatal(reason)
	}
}
func TestQuotaRealHostUsageIsScopedAndSanitized(t *testing.T) {
	if _, e := exec.LookPath("bun"); e != nil {
		t.Skip("Bun is optional; real-host fixture requires Bun")
	}
	root, r := quotaFixture(t)
	script := `export default async () => ({auth:{provider:'quota',methods:[],loader:async()=>({}),usage:async(getAuth)=>{
const a=await getAuth();if(a.access!=='PRIVATE-ONE')throw new Error('wrong scope');
return {user:'PRIVATE-USER',token:'PRIVATE-TOKEN',windows:[{name:'day',used:25,apiKey:'PRIVATE-KEY'}]};}}});`
	if e := Atomic(r.Plugins[0].Path, []byte(script), 0600); e != nil {
		t.Fatal(e)
	}
	v, e := QuotaRefresh(root, "quota#one")
	if e != nil || v["ok"] != true {
		t.Fatal(v, e)
	}
	c, e := QuotaList(root)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("credential returned")
	}
	cache, _ := readQuotaCache(root)
	w := cache.Accounts["quota#one"].Windows
	if len(w) != 1 || w[0].Used == nil || *w[0].Used != 25 {
		t.Fatal(w)
	}
}
func TestQuotaRealHostAbortDrainsBeforeReturning(t *testing.T) {
	if _, e := exec.LookPath("bun"); e != nil {
		t.Skip("Bun is optional; real-host fixture requires Bun")
	}
	root, r := quotaFixture(t)
	script := `import {writeFile} from 'node:fs/promises';export default async (input) => ({auth:{provider:'quota',methods:[],loader:async()=>({}),usage:async(_a,_p,{signal})=>{
await new Promise(resolve=>{if(signal.aborted)resolve();else signal.addEventListener('abort',resolve,{once:true})});
await new Promise(resolve=>setTimeout(resolve,50));await writeFile(input.directory+'/drained','done');return {windows:[]};}}});`
	if e := Atomic(r.Plugins[0].Path, []byte(script), 0600); e != nil {
		t.Fatal(e)
	}
	h, e := startHost(root, r.Plugins[0].Source, time.Now().Add(3*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	var out any
	e = h.callUntil("usage", map[string]any{"provider": "quota", "account": "quota#one"}, &out, time.Now().Add(200*time.Millisecond))
	h.Close()
	if !errors.Is(e, errHostDeadline) {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(root, "drained")); e != nil || string(b) != "done" {
		t.Fatal("abort killed pending checkpoint", e)
	}
}
