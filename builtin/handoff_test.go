package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeOwnerFixture(t *testing.T) {
	if os.Getenv("ZCALLD_TEST_NATIVE_OWNER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func ownerFixture(t *testing.T, source string) (string, *exec.Cmd) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native process identity verification requires Linux")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeOwnerFixture$")
	cmd.Env = append(os.Environ(), "ZCALLD_TEST_NATIVE_OWNER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	start, alive, err := linuxProcess(cmd.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("fixture process was not observed: %v", err)
	}
	boot, ns, err := ownerEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	manifest := OwnerManifest{Version: 1, Platform: "linux", BootID: boot, PIDNamespace: ns, SourceAuthFile: source, AccountHash: hash("fixture-account"), CapturedAt: time.Now().UTC().Format(time.RFC3339), Holders: []OwnerProcess{{PID: cmd.Process.Pid, StartTicks: start}}}
	path := filepath.Join(t.TempDir(), "owners.json")
	put(t, path, manifest)
	return path, cmd
}

func claimFixture(t *testing.T, root string, paths activePaths, key string) {
	t.Helper()
	manifest, child := ownerFixture(t, filepath.Join(paths.CodexHome, "auth.json"))
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if _, err := Handoff(root, key, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestExternalRollbackNeverOverwritesNativeRefresh(t *testing.T) {
	root, source, paths := setup(t)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var raw object
	read(filepath.Join(paths.CodexHome, "auth.json"), &raw)
	obj(raw["tokens"])["refresh_token"] = "fixture-native-rotation"
	put(t, filepath.Join(paths.CodexHome, "auth.json"), raw)
	before, _ := os.ReadFile(filepath.Join(source, "logins.json"))
	activeBefore, _ := os.ReadFile(filepath.Join(paths.CodexHome, "auth.json"))
	result, err := Sync(root, source)
	if err != nil {
		t.Fatal(err)
	}
	if result["externally_managed_accounts"] != 1 {
		t.Fatal("external owner was not reported")
	}
	after, _ := os.ReadFile(filepath.Join(source, "logins.json"))
	activeAfter, _ := os.ReadFile(filepath.Join(paths.CodexHome, "auth.json"))
	if string(before) != string(after) || string(activeBefore) != string(activeAfter) {
		t.Fatal("rollback overwrote an externally managed source")
	}
}

func TestHandoffRefusesLiveHolderThenCopiesLatestAccount(t *testing.T) {
	root, source, paths := setup(t)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	key := ""
	for candidate := range auth {
		if strings.HasPrefix(candidate, "builtin:codex#") {
			key = candidate
		}
	}
	manifest, child := ownerFixture(t, filepath.Join(paths.CodexHome, "auth.json"))
	before, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	if _, err := Handoff(root, key, manifest); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("live native holder was not protected: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "plugin-auth.json"))
	if string(before) != string(after) {
		t.Fatal("refused handoff changed credential state")
	}
	var raw object
	read(filepath.Join(paths.CodexHome, "auth.json"), &raw)
	obj(raw["tokens"])["refresh_token"] = "fixture-native-last-rotation"
	put(t, filepath.Join(paths.CodexHome, "auth.json"), raw)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	result, err := Handoff(root, key, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result["network_requests"] != 0 || result["verified_exited_holders"] != 1 {
		t.Fatal("handoff did not record a verified no-network transition")
	}
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	entry := obj(auth[key])
	if entry["refreshOwner"] != "host" || entry["refresh"] != "fixture-native-last-rotation" {
		t.Fatal("handoff did not copy the latest native credential")
	}
	if _, ok := entry["sourceAuthFile"]; ok {
		t.Fatal("host-owned auth still advertises a borrowed source")
	}
	if _, err = importPaths(root, source, paths); err == nil {
		t.Fatal("reimport silently reversed a completed handoff")
	}
	entry["refresh"] = "fixture-host-rotation"
	entry["access"] = "fixture-host-access"
	put(t, filepath.Join(root, "plugin-auth.json"), auth)
	if _, err = Sync(root, source); err != nil {
		t.Fatal(err)
	}
	read(filepath.Join(paths.CodexHome, "auth.json"), &raw)
	if obj(raw["tokens"])["refresh_token"] != "fixture-host-rotation" {
		t.Fatal("rollback used a fingerprint from before the native owner's last refresh")
	}
}

func TestHandoffBindsAccountAndProcessBirth(t *testing.T) {
	root, source, paths := setup(t)
	if _, err := importPaths(root, source, paths); err != nil {
		t.Fatal(err)
	}
	var auth object
	read(filepath.Join(root, "plugin-auth.json"), &auth)
	key := ""
	for candidate := range auth {
		if strings.HasPrefix(candidate, "builtin:codex#") {
			key = candidate
		}
	}
	manifest, child := ownerFixture(t, filepath.Join(paths.CodexHome, "auth.json"))
	var owners OwnerManifest
	read(manifest, &owners)
	owners.Holders[0].StartTicks++ // Same PID, different birth identity is not the recorded holder.
	if err := owners.allExited(); err != nil {
		t.Fatalf("PID reuse was mistaken for the old process: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	var raw object
	read(filepath.Join(paths.CodexHome, "auth.json"), &raw)
	obj(raw["tokens"])["account_id"] = "fixture-different-account"
	put(t, filepath.Join(paths.CodexHome, "auth.json"), raw)
	if _, err := Handoff(root, key, manifest); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("handoff accepted a different source account: %v", err)
	}
}
