package community

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuotaRealHostFreshUsesOnlySelectedAccountAndPlugin(t *testing.T) {
	if _, e := exec.LookPath("bun"); e != nil {
		t.Skip("Bun is optional; real-host fixture requires Bun")
	}
	root, r := quotaFixture(t)
	bad := filepath.Join(root, "must-not-load.mjs")
	if e := Atomic(bad, []byte("throw new Error('UNRELATED-MODULE-LOADED')"), 0600); e != nil {
		t.Fatal(e)
	}
	r.Plugins = append(r.Plugins, Entry{Name: "unrelated", Kind: "community", Source: "npm:unrelated@1.0.0", Path: bad, Enabled: true})
	if e := Save(root, r); e != nil {
		t.Fatal(e)
	}
	if e := Atomic(filepath.Join(root, "plugin-auth.json"), []byte(`{"quota#one":{"type":"oauth","access":"OLD","refresh":"ROTATING","expires":1},"quota#two":{"type":"api","key":"UNRELATED-UNCHANGED"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	script := `import {appendFile} from 'node:fs/promises';
export default async(input)=>({auth:{provider:'quota',methods:[],loader:async()=>({}),
refresh:async(auth)=>{if(auth.access!=='OLD')throw new Error('wrong account');
await appendFile(input.directory+'/renew-count','1');return {access:'NEW',refresh:'NEXT',expires:4102444800000};},
usage:async(getAuth)=>{if((await getAuth()).access!=='NEW')throw new Error('fresh was skipped');
return {windows:[{name:'day',used:40}]};}}});`
	if e := Atomic(r.Plugins[0].Path, []byte(script), 0600); e != nil {
		t.Fatal(e)
	}
	value, e := QuotaRefresh(root, "quota#one")
	if e != nil || value["ok"] != true {
		all, _, _ := readAuthFile(root)
		count, _ := os.ReadFile(filepath.Join(root, "renew-count"))
		t.Fatalf("fresh fixture failed: result=%v err=%v renew_calls=%d new_access=%v", value, e, len(count), all["quota#one"].(map[string]any)["access"] == "NEW")
	}
	all, _, e := readAuthFile(root)
	if e != nil {
		t.Fatal(e)
	}
	if all["quota#one"].(map[string]any)["access"] != "NEW" || all["quota#two"].(map[string]any)["key"] != "UNRELATED-UNCHANGED" {
		t.Fatal("fresh did not preserve account scope")
	}
	count, e := os.ReadFile(filepath.Join(root, "renew-count"))
	if e != nil || string(count) != "1" {
		t.Fatal("renewal replayed", e)
	}
	cache, e := os.ReadFile(filepath.Join(root, quotaCacheFile))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(cache), "NEW") || strings.Contains(string(cache), "NEXT") {
		t.Fatal("credential reached cache")
	}
}
