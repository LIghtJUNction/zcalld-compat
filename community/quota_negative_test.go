package community

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestQuotaRealHostRejectsStaleCapabilityAndExpiredReadings(t *testing.T) {
	if _, e := exec.LookPath("bun"); e != nil {
		t.Skip("Bun is optional; real-host fixture requires Bun")
	}
	for _, test := range []struct{ name, hook string }{
		{"missing_hook", ""},
		{"provider_error", ",usage:async()=>{throw new Error('PRIVATE-ERROR')}"},
		{"expired", ",usage:async()=>({signIn:'expired',windows:[{used:10}]})"},
		{"too_many_windows", ",usage:async()=>({windows:Array.from({length:65},()=>({used:10}))})"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, r := quotaFixture(t)
			script := "export default async()=>({auth:{provider:'quota',methods:[],loader:async()=>({})" + test.hook + "}});"
			if e := Atomic(r.Plugins[0].Path, []byte(script), 0600); e != nil {
				t.Fatal(e)
			}
			v, e := QuotaRefresh(root, "quota#one")
			if e != nil || v["ok"] != false || v["error"] != "provider_error" {
				t.Fatal(v, e)
			}
			if _, e = os.Stat(filepath.Join(root, quotaCacheFile)); !os.IsNotExist(e) {
				t.Fatal("invalid quota was cached")
			}
		})
	}
}
