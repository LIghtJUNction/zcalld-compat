package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/internal/redact"
)

func readAudit(t *testing.T, dir string) []auditRecord {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []auditRecord
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			var r auditRecord
			if err = json.Unmarshal(line, &r); err != nil {
				t.Fatalf("invalid audit record: %v", err)
			}
			out = append(out, r)
		}
	}
	return out
}
func countsAt(t *testing.T, dir, day string) map[string]checkedCount {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "checked-"+day+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var counts map[string]checkedCount
	if err = json.Unmarshal(b, &counts); err != nil {
		t.Fatal(err)
	}
	return counts
}
func writePrivate(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(path, 0600); err != nil {
		t.Fatal(err)
	}
}
func sampleHits() auditHits {
	var h auditHits
	h.add(redact.Hit{Placeholder: "{{EMAIL_synthetic}}", Kind: "EMAIL", By: "rule", Head: "s", Len: 24})
	return h
}
func TestAuditMaskMetadataAndChecked(t *testing.T) {
	e := testEngine(t, map[string]any{"redact": true, "redactPersonal": true})
	c := Context{Agent: "synthetic-agent", Session: "test-session", What: "/v1/chat/completions"}
	secret := "unique.person@acme.io"
	body, _ := json.Marshal(map[string]any{"model": "private-model-name", "messages": []any{map[string]any{"role": "user", "content": secret + " and " + secret}}})
	r, err := e.MaskWithContext(body, "", c)
	if err != nil || r.Count != 2 || r.AuditError != "" || bytes.Contains(r.Body, []byte(secret)) {
		t.Fatalf("mask/audit failed: %v", err)
	}
	records := readAudit(t, e.audit.dir)
	if len(records) != 1 {
		t.Fatalf("append was not immediate: %d", len(records))
	}
	rec := records[0]
	if rec.Agent != c.Agent || rec.Session != c.Session || rec.What != c.What || rec.N != 2 ||
		len(rec.Values) != 1 || rec.Values[0].N != 2 || rec.Values[0].By == "" {
		t.Fatalf("bad metadata: %+v", rec)
	}
	b, _ := os.ReadFile(filepath.Join(e.audit.dir, rec.Time.Local().Format(auditDay)+".jsonl"))
	if bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte("private-model-name")) {
		t.Fatal("private request values reached disk")
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal(bytes.TrimSpace(b), &obj); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"provider", "model", "route", "took", "sent", "snips", "back", "body"} {
		if _, ok := obj[key]; ok {
			t.Fatalf("unavailable/private field persisted: %s", key)
		}
	}
	if _, err = e.MaskWithContext([]byte(`{"messages":[{"role":"user","content":"ordinary harmless text"}]}`), "", c); err != nil {
		t.Fatal(err)
	}
	if len(readAudit(t, e.audit.dir)) != 1 {
		t.Fatal("no-hit request produced an event")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	got := countsAt(t, e.audit.dir, rec.Time.Local().Format(auditDay))[c.Agent]
	if got != (checkedCount{Looked: 2, Masked: 1}) {
		t.Fatalf("wrong request counters: %+v", got)
	}
	entries, _ := os.ReadDir(e.audit.dir)
	for _, entry := range entries {
		if !privatefile.Check(filepath.Join(e.audit.dir, entry.Name())) {
			t.Fatalf("audit file is not private: %s", entry.Name())
		}
	}
}
func TestAuditOffAndEmptyPreflightDoNotWrite(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			e := testEngine(t, map[string]any{"redactPersonal": true, "zcalldEnabled": !off})
			cases := []string{`{"messages":[]}`, `{"messages":[{"role":"user","content":""}]}`, `{"input":"","model":"only-a-model"}`, `{}`}
			if off {
				cases = append(cases, `{"input":"person@acme.io"}`)
			}
			for _, body := range cases {
				if _, err := e.Mask([]byte(body), ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(e.audit.dir); !os.IsNotExist(err) {
				t.Fatal("disabled/empty probe wrote audit state")
			}
		})
	}
}
func TestAuditWarnAndBlockEvents(t *testing.T) {
	for _, action := range []string{"warn", "block"} {
		t.Run(action, func(t *testing.T) {
			e := testEngine(t, map[string]any{"redactPersonal": true, "redactActions": map[string]string{"EMAIL": action}})
			r, err := e.Mask([]byte(`{"messages":[{"content":"test.person@acme.io"}]}`), "")
			if err != nil || r.AuditError != "" {
				t.Fatal(err)
			}
			records := readAudit(t, e.audit.dir)
			if len(records) != 1 || len(records[0].Values) != 1 || records[0].Values[0].Action != action {
				t.Fatal("action metadata missing")
			}
			if action == "block" && (!r.Blocked || len(r.Body) != 0 || !records[0].Blocked || records[0].What != "blocked") {
				t.Fatal("blocked request escaped")
			}
			if action == "warn" && (r.Blocked || records[0].N != 0 || !bytes.Contains(r.Body, []byte("test.person@acme.io"))) {
				t.Fatal("warn semantics changed")
			}
		})
	}
}
func TestAuditFailureKeepsMaskAndCanFlushAfterRecovery(t *testing.T) {
	e := testEngine(t, map[string]any{"redactPersonal": true})
	writePrivate(t, e.audit.dir, []byte("unavailable-directory"))
	secret := "test.person@acme.io"
	body := []byte(`{"messages":[{"content":"` + secret + `"}]}`)
	r, err := e.Mask(body, "")
	if err != nil || r.Count != 1 || bytes.Contains(r.Body, []byte(secret)) || r.AuditError != errAudit.Error() {
		t.Fatal("audit failure bypassed masking or leaked an error")
	}
	if e.Close() == nil {
		t.Fatal("failed flush was reported as successful")
	}
	if err = os.Remove(e.audit.dir); err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	got := countsAt(t, e.audit.dir, time.Now().Local().Format(auditDay))["other"]
	if got != (checkedCount{Looked: 1, Masked: 1}) {
		t.Fatal("recovery lost or duplicated counter")
	}
	if len(readAudit(t, e.audit.dir)) != 0 {
		t.Fatal("failed append was replayed")
	}
}
func TestAuditContextAndOutputBounds(t *testing.T) {
	e := testEngine(t, map[string]any{"redactPersonal": true})
	if _, err := e.MaskWithContext([]byte(`{"input":"ordinary text"}`), "", Context{Agent: strings.Repeat("a", 513)}); err == nil {
		t.Fatal("oversized context accepted")
	}
	h := sampleHits()
	h.values[0].By = "known"
	h.add(redact.Hit{Placeholder: "{{EMAIL_synthetic}}", Kind: "EMAIL", By: "rule", Action: "block"})
	for i := 0; i < auditValueLimit+5; i++ {
		h.add(redact.Hit{Placeholder: fmt.Sprintf("{{EMAIL_%d}}", i), Kind: "EMAIL", By: "rule", Len: 20})
	}
	if len(h.values) != auditValueLimit || h.omitted != 6 || h.values[0].N != 2 || h.values[0].By != "rule" || h.values[0].Action != "block" {
		t.Fatal("bounded aggregate changed merge semantics")
	}
	if err := e.audit.note(Context{}, h, auditValueLimit+7, false); err != nil {
		t.Fatal(err)
	}
	records := readAudit(t, e.audit.dir)
	if len(records) != 1 || records[0].OmittedHits != 6 {
		t.Fatal("overflow was not explicit")
	}
	data, _ := json.Marshal(records[0])
	if len(data) > auditRecordLimit {
		t.Fatal("record exceeded memory/disk bound")
	}
}
func TestAuditEncodedRecordLimitReportsOmittedHits(t *testing.T) {
	a := newAuditStore(t.TempDir())
	t.Cleanup(func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	})
	var h auditHits
	for i := 0; i < 700; i++ {
		h.add(redact.Hit{
			Placeholder: strings.Repeat("\x01", 500) + fmt.Sprintf("%012d", i),
			Kind:        strings.Repeat("\x02", 128), By: strings.Repeat("\x03", 64),
			Head: strings.Repeat("\x04", 32), Len: 100,
		})
	}
	if err := a.note(Context{}, h, 700, false); err != nil {
		t.Fatal(err)
	}
	records := readAudit(t, a.dir)
	if len(records) != 1 || records[0].OmittedHits == 0 || records[0].N != 700 {
		t.Fatal("encoded size cap lost the total or hid truncation")
	}
	data, _ := json.Marshal(records[0])
	if len(data) > auditRecordLimit || len(records[0].Values)+records[0].OmittedHits != 700 {
		t.Fatal("encoded record bound was not enforced")
	}
}

func TestAuditConcurrentStoresMergeCounters(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			a := newAuditStore(dir)
			a.lockWait = 5 * time.Second
			a.now = func() time.Time { return now }
			for j := 0; j < 12; j++ {
				if err := a.note(Context{Agent: "same-agent"}, sampleHits(), 1, false); err != nil {
					errs <- err
					return
				}
			}
			errs <- a.close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	records := readAudit(t, dir)
	if len(records) != 96 {
		t.Fatalf("lost concurrent appends: %d", len(records))
	}
	keys := map[int64]bool{}
	for _, r := range records {
		if keys[r.Key] {
			t.Fatal("duplicate cross-store key")
		}
		keys[r.Key] = true
	}
	if got := countsAt(t, dir, now.Local().Format(auditDay))["same-agent"]; got != (checkedCount{Looked: 96, Masked: 96}) {
		t.Fatalf("lost concurrent counts: %+v", got)
	}
}
func TestAuditProcessHelper(t *testing.T) {
	dir := os.Getenv("ZCALLD_AUDIT_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess fixture")
	}
	a := newAuditStore(dir)
	a.lockWait = 5 * time.Second
	a.now = func() time.Time { return time.Unix(1795136400, 0) }
	for i := 0; i < 8; i++ {
		if err := a.note(Context{Agent: "child"}, sampleHits(), 1, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
}
func TestAuditConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := 0; i < 4; i++ {
		cmd := exec.Command(exe, "-test.run=^TestAuditProcessHelper$")
		cmd.Env = append(os.Environ(), "ZCALLD_AUDIT_TEST_DIR="+dir)
		out := new(bytes.Buffer)
		cmd.Stdout = out
		cmd.Stderr = out
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		outputs = append(outputs, out)
	}
	for i, cmd := range cmds {
		if err = cmd.Wait(); err != nil {
			t.Fatalf("writer process: %v\n%s", err, outputs[i].String())
		}
	}
	records := readAudit(t, dir)
	if len(records) != 32 {
		t.Fatalf("cross-process append loss: %d", len(records))
	}
	keys := map[int64]bool{}
	for _, r := range records {
		if keys[r.Key] {
			t.Fatal("cross-process key collision")
		}
		keys[r.Key] = true
	}
	if got := countsAt(t, dir, records[0].Time.Local().Format(auditDay))["child"]; got != (checkedCount{Looked: 32, Masked: 32}) {
		t.Fatal("cross-process count loss")
	}
}
func TestAuditLockHasDeadline(t *testing.T) {
	dir := t.TempDir()
	f, err := openAuditFile(auditLockPath(dir), os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	release, err := auditLock(f, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	a := newAuditStore(dir)
	a.lockWait = 20 * time.Millisecond
	start := time.Now()
	err = a.note(Context{}, sampleHits(), 1, false)
	elapsed := time.Since(start)
	release()
	if err == nil || elapsed > time.Second {
		t.Fatalf("audit lock was not bounded: %v %s", err, elapsed)
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
}
func TestAuditDailyRetentionAndCheckedBudget(t *testing.T) {
	a := newAuditStore(t.TempDir())
	a.keepBytes = 9
	now := time.Now()
	for _, age := range []int{31, 30, 1, 0} {
		day := now.Local().AddDate(0, 0, -age).Format(auditDay)
		writePrivate(t, filepath.Join(a.dir, day+".jsonl"), []byte("1234"))
		writePrivate(t, filepath.Join(a.dir, "checked-"+day+".json"), []byte("{}"))
	}
	writePrivate(t, filepath.Join(a.dir, "unrelated.txt"), []byte("keep"))
	if err := a.withLock(func(_ *os.File) error { return a.prune(now) }); err != nil {
		t.Fatal(err)
	}
	for _, age := range []int{31, 30} {
		if _, err := os.Stat(filepath.Join(a.dir, now.Local().AddDate(0, 0, -age).Format(auditDay)+".jsonl")); !os.IsNotExist(err) {
			t.Fatal("old/budget day retained")
		}
	}
	for _, age := range []int{1, 0} {
		b, err := os.ReadFile(filepath.Join(a.dir, now.Local().AddDate(0, 0, -age).Format(auditDay)+".jsonl"))
		if err != nil || string(b) != "1234" {
			t.Fatal("recent day was truncated")
		}
	}
	if _, err := os.Stat(filepath.Join(a.dir, "checked-"+now.Local().AddDate(0, 0, -30).Format(auditDay)+".json")); err != nil {
		t.Fatal("checked file incorrectly shared JSONL size budget")
	}
	if _, err := os.Stat(filepath.Join(a.dir, "checked-"+now.Local().AddDate(0, 0, -31).Format(auditDay)+".json")); !os.IsNotExist(err) {
		t.Fatal("expired checked file retained")
	}
	if b, _ := os.ReadFile(filepath.Join(a.dir, "unrelated.txt")); string(b) != "keep" {
		t.Fatal("unrelated file changed")
	}
}
func TestAuditCounterFailurePreservesFileAndRetriesOnce(t *testing.T) {
	a := newAuditStore(t.TempDir())
	day := time.Now().Local().Format(auditDay)
	path := filepath.Join(a.dir, "checked-"+day+".json")
	writePrivate(t, path, []byte("{incomplete"))
	if err := a.note(Context{Agent: "same"}, auditHits{}, 0, false); err != nil {
		t.Fatal(err)
	}
	if a.close() == nil {
		t.Fatal("corrupt count file silently overwritten")
	}
	if b, _ := os.ReadFile(path); string(b) != "{incomplete" {
		t.Fatal("corrupt file was destroyed")
	}
	writePrivate(t, path, []byte(`{"same":{"looked":4,"masked":2}}`))
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	if got := countsAt(t, a.dir, day)["same"]; got != (checkedCount{Looked: 5, Masked: 2}) {
		t.Fatal("counter retry lost or duplicated a delta")
	}
}
func TestAuditTimerFlushesWithoutShutdown(t *testing.T) {
	a := newAuditStore(t.TempDir())
	a.every = 10 * time.Millisecond
	t.Cleanup(func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	})
	if err := a.note(Context{}, auditHits{}, 0, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.dir, "checked-"+time.Now().Local().Format(auditDay)+".json")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(`"looked":1`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timer did not flush counters")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestAuditRebuildsKeyAfterLockIsOmitted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	future := now.UnixNano() + 1000
	day := now.Local().Format(auditDay)
	seed := fmt.Sprintf("{\"key\":%d}\n{\"key\":%d}\n", future, future-1)
	writePrivate(t, filepath.Join(dir, day+".jsonl"), []byte(seed))
	for i := int64(1); i <= 2; i++ {
		a := newAuditStore(dir)
		a.now = func() time.Time { return now }
		if err := a.note(Context{}, sampleHits(), 1, false); err != nil {
			t.Fatal(err)
		}
		if err := a.close(); err != nil {
			t.Fatal(err)
		}
		records := readAudit(t, dir)
		if records[len(records)-1].Key != future+i {
			t.Fatal("history did not reconstruct largest existing key")
		}
		if err := os.Remove(auditLockPath(dir)); err != nil {
			t.Fatal(err)
		}
	}
}

// Directory replacement must not make a held old lock authorize a new path.
func TestAuditRejectsReplacedLockOrDirectoryIdentity(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "history")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openAuditFile(auditLockPath(dir), os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !sameAuditLocation(dir, before, f) {
		t.Fatal("fresh lock identity rejected")
	}
	if err = os.Rename(dir, filepath.Join(base, "previous")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if sameAuditLocation(dir, before, f) {
		t.Fatal("directory exchange reused old identity")
	}
	current, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameAuditLocation(dir, current, f) {
		t.Fatal("stable external lock did not survive directory exchange")
	}
	if err = os.Rename(auditLockPath(dir), auditLockPath(dir)+".old"); err != nil {
		t.Skip("platform prevents renaming an open lock file")
	}
	writePrivate(t, auditLockPath(dir), nil)
	if sameAuditLocation(dir, current, f) {
		t.Fatal("replacement lock accepted an old held descriptor")
	}
}

func TestAuditRefusesSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "unchanged")
	writePrivate(t, outside, []byte("original"))
	link := auditLockPath(dir)
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink fixture not permitted on this platform")
	}
	a := newAuditStore(dir)
	if a.note(Context{}, sampleHits(), 1, false) == nil {
		t.Fatal("symlink was followed")
	}
	if a.close() == nil {
		t.Fatal("symlink flush was reported successful")
	}
	if b, _ := os.ReadFile(outside); string(b) != "original" {
		t.Fatal("symlink target was modified")
	}
}
