package community

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalLifecycleAndPrivateOptions(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "fixture.mjs")
	os.WriteFile(source, []byte("export default async()=>({})"), 0600)
	root := filepath.Join(d, "extensions")
	result, err := Install(root, InstallOptions{Source: source, Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	plugin := result["plugin"].(map[string]any)
	if plugin["enabled"] != false || plugin["kind"] != "community" {
		t.Fatal("new code was enabled without an explicit action")
	}
	old := plugin["path"].(string)
	if strings.Contains(old, ".stage-") {
		t.Fatal("staging path escaped")
	}
	options := filepath.Join(d, "options.json")
	os.WriteFile(options, []byte("{\"token\":\"synthetic-private-value\"}"), 0600)
	result, err = Change(root, "fixture", "config", options)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), "synthetic-private-value") {
		t.Fatal("options leaked in public result")
	}
	if _, err = Change(root, "fixture", "enable", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = Change(root, "fixture", "disable", ""); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(source, []byte("export default async()=>({version:2})"), 0600)
	result, err = Install(root, InstallOptions{Name: "fixture", Update: true})
	if err != nil {
		t.Fatal(err)
	}
	if result["plugin"].(map[string]any)["path"] == old {
		t.Fatal("update overwrote an active generation")
	}
	if _, err = os.Stat(old); err != nil {
		t.Fatal("active older generation removed while requests may use it")
	}
	if _, err = Change(root, "fixture", "remove", ""); err != nil {
		t.Fatal(err)
	}
	r, err := Read(root)
	if err != nil || len(r.Plugins) != 0 {
		t.Fatal("remove did not clear registry")
	}
}
func TestNativeChecksumAndImmutableGitRequirement(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "fixture.so")
	os.WriteFile(source, []byte("\x7fELFsynthetic-native-not-executed"), 0600)
	root := filepath.Join(d, "extensions")
	if _, err := Install(root, InstallOptions{Source: "github:example/plugin", Name: "git", Revision: "main"}); err == nil {
		t.Fatal("mutable Git revision accepted")
	}
	if _, err := Install(root, InstallOptions{Source: source, Name: "fixture", SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("bad checksum accepted")
	}
	result, err := Install(root, InstallOptions{Source: source, Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(root, "fixture"); err != nil {
		t.Fatal(err)
	}
	path := result["plugin"].(map[string]any)["path"].(string)
	os.WriteFile(path, []byte("changed"), 0600)
	if _, err = Change(root, "fixture", "enable", ""); err == nil {
		t.Fatal("changed native package enabled")
	}
}
func TestPrivateSourceSymlinkCannotEscape(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "source")
	os.Mkdir(source, 0700)
	outside := filepath.Join(d, "secret")
	os.WriteFile(outside, []byte("fixture"), 0600)
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, _, err := copyPrivateTree(source, filepath.Join(d, "copy")); err == nil {
		t.Fatal("external symlink accepted")
	}
}
func TestNoPackageLifecycleScriptRuns(t *testing.T) {
	d := t.TempDir()
	source := filepath.Join(d, "source")
	os.Mkdir(source, 0700)
	os.WriteFile(filepath.Join(source, "package.json"), []byte("{\"name\":\"zcalld-local-fixture\",\"version\":\"1.0.0\",\"main\":\"index.mjs\",\"scripts\":{\"postinstall\":\"touch MUST_NOT_RUN\"}}"), 0600)
	os.WriteFile(filepath.Join(source, "index.mjs"), []byte("export default async()=>({})"), 0600)
	result, err := Install(filepath.Join(d, "extensions"), InstallOptions{Source: source, Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	target := result["plugin"].(map[string]any)["path"].(string)
	if _, err = os.Stat(filepath.Join(target, "MUST_NOT_RUN")); !os.IsNotExist(err) {
		t.Fatal("package lifecycle script ran")
	}
}
