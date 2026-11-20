package privacy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/internal/redact"
)

func firstTestStore(t *testing.T) (*auditStore, string) {
	t.Helper()
	root := t.TempDir()
	if err := privatefile.Protect(root, 0700); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(root, "inbox")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	a := newAuditStore(filepath.Join(root, "history"))
	a.desktopInbox = inbox
	a.every = time.Hour
	a.lockWait = 2 * time.Second
	t.Cleanup(func() { _ = a.close() })
	return a, inbox
}
func firstTestHits(p, action string) auditHits {
	var h auditHits
	h.add(redact.Hit{Placeholder: p, Kind: "EMAIL", By: "known", Head: "private", Len: 24, Action: action})
	return h
}
func firstTestNote(t *testing.T, a *auditStore, p, action string) {
	t.Helper()
	h := firstTestHits(p, action)
	if err := a.note(Context{Agent: "codex"}, h, 1, action == "block"); err != nil {
		t.Fatal(err)
	}
}
func firstTestEvents(t *testing.T, inbox string) []firstEvent {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(inbox, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var events []firstEvent
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Kind  int        `json:"kind"`
			Event firstEvent `json:"event"`
		}
		if json.Unmarshal(data, &e) != nil || e.Kind != 2 || e.Event.Version != 1 ||
			!firstHex(e.Event.EventID, 32) {
			t.Fatal("invalid pure-count event")
		}
		events = append(events, e.Event)
	}
	return events
}
func firstTestCount(events []firstEvent) int {
	total := 0
	for _, e := range events {
		for _, c := range e.Counts {
			total += c.NewValues
		}
	}
	return total
}

func TestFirstDisabledHasNoIndexOrQueueEffects(t *testing.T) {
	a, inbox := firstTestStore(t)
	a.desktopInbox = ""
	firstTestNote(t, a, "{{EMAIL_disabled}}", "")
	if _, err := os.Lstat(firstIndexPath(a.dir)); !os.IsNotExist(err) {
		t.Fatal("disabled notification created an index")
	}
	if len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("disabled notification published")
	}
	a.desktopInbox = inbox
	if err := a.note(Context{}, auditHits{}, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(firstIndexPath(a.dir)); !os.IsNotExist(err) {
		t.Fatal("no-hit request created an index")
	}
}

func TestFirstDeduplicatesRequestsAndRestart(t *testing.T) {
	a, inbox := firstTestStore(t)
	h := firstTestHits("{{EMAIL_repeated}}", "")
	h.add(redact.Hit{Placeholder: "{{EMAIL_repeated}}", Kind: "EMAIL", By: "known", Len: 24})
	if err := a.note(Context{Agent: "codex"}, h, 2, false); err != nil {
		t.Fatal(err)
	}
	firstTestNote(t, a, "{{EMAIL_repeated}}", "")
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	b := newAuditStore(a.dir)
	b.desktopInbox = inbox
	b.every = time.Hour
	defer b.close()
	firstTestNote(t, b, "{{EMAIL_repeated}}", "")
	firstTestNote(t, b, "{{EMAIL_second}}", "")
	events := firstTestEvents(t, inbox)
	if len(events) != 2 || firstTestCount(events) != 2 {
		t.Fatalf("repeat/restart produced %d events, %d values", len(events), firstTestCount(events))
	}
	for _, e := range events {
		if e.Agent != "codex" || len(e.Counts) != 1 || e.Counts[0].Kind != "EMAIL" || e.Counts[0].NewValues != 1 {
			t.Fatal("count is occurrences instead of distinct values")
		}
	}
	index := firstIndexPath(a.dir)
	entries, err := os.ReadDir(index)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(index, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"EMAIL_repeated", "EMAIL_second", "private", "codex"} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Fatal("index persisted a raw label or value")
			}
		}
	}
}

func TestFirstSeedsMigratedHistoryAndWarnsStayKnown(t *testing.T) {
	a, inbox := firstTestStore(t)
	a.desktopInbox = ""
	firstTestNote(t, a, "{{EMAIL_prior_mask}}", "")
	firstTestNote(t, a, "{{EMAIL_prior_warn}}", "warn")
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	b := newAuditStore(a.dir)
	b.desktopInbox = inbox
	b.every = time.Hour
	defer b.close()
	firstTestNote(t, b, "{{EMAIL_prior_mask}}", "")
	firstTestNote(t, b, "{{EMAIL_prior_warn}}", "block")
	firstTestNote(t, b, "{{EMAIL_new_warn}}", "warn")
	firstTestNote(t, b, "{{EMAIL_new_warn}}", "")
	firstTestNote(t, b, "{{EMAIL_new_block}}", "block")
	if got := firstTestEvents(t, inbox); len(got) != 1 || firstTestCount(got) != 1 {
		t.Fatal("seed/warn observation was announced as a new masked value")
	}
}

func TestFirstHistoryAppendWithOlderKeyIsSeededSilently(t *testing.T) {
	a, inbox := firstTestStore(t)
	firstTestNote(t, a, "{{EMAIL_initial}}", "")
	r := auditRecord{Key: 1, Time: a.now(), What: "generation", N: 1, Values: firstTestHits("{{EMAIL_external}}", "").values}
	data, _ := json.Marshal(r)
	path := filepath.Join(a.dir, r.Time.Local().Format(auditDay)+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	firstTestNote(t, a, "{{EMAIL_external}}", "")
	if len(firstTestEvents(t, inbox)) != 1 {
		t.Fatal("older external record key bypassed the history cursor")
	}
}

func TestFirstCrashRecoveryDoesNotReplayDurableHistory(t *testing.T) {
	for _, phase := range []string{"history", "bucket", "manifest"} {
		t.Run(phase, func(t *testing.T) {
			a, inbox := firstTestStore(t)
			err := a.withLock(func(lock *os.File) error {
				i, err := prepareFirst(a.dir, make(map[string]firstVerification))
				if err != nil {
					return err
				}
				r := auditRecord{Time: a.now(), What: "generation", N: 1, Values: firstTestHits("{{EMAIL_interrupted}}", "").values}
				if err = a.append(lock, &r, true); err != nil {
					return err
				}
				if phase == "bucket" {
					_, err = i.add(map[[32]byte]struct{}{firstDigest(r.Values[0].Placeholder): {}})
				}
				if phase == "manifest" {
					_, err = i.record(a.dir, r)
				}
				// Simulate termination without ever invoking the publisher.
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			firstTestNote(t, a, "{{EMAIL_interrupted}}", "")
			if len(firstTestEvents(t, inbox)) != 0 {
				t.Fatal("interrupted observation was replayed")
			}
			firstTestNote(t, a, "{{EMAIL_later}}", "")
			if len(firstTestEvents(t, inbox)) != 1 {
				t.Fatal("recovery suppressed a genuinely new value")
			}
		})
	}
}

func TestFirstCorruptIndexSuppressesWithoutDroppingAudit(t *testing.T) {
	for _, damage := range []string{"manifest", "bucket", "missing_bucket", "unsafe_node"} {
		t.Run(damage, func(t *testing.T) {
			a, inbox := firstTestStore(t)
			firstTestNote(t, a, "{{EMAIL_before_damage}}", "")
			index := firstIndexPath(a.dir)
			switch damage {
			case "manifest":
				writePrivate(t, filepath.Join(index, "state.json"), []byte("{"))
			case "bucket", "missing_bucket":
				paths, _ := filepath.Glob(filepath.Join(index, "*.bin"))
				if len(paths) != 1 {
					t.Fatal("bad fixture")
				}
				if damage == "missing_bucket" {
					if os.Remove(paths[0]) != nil {
						t.Fatal("remove fixture")
					}
				} else {
					b, err := os.ReadFile(paths[0])
					if err != nil {
						t.Fatal(err)
					}
					b[len(b)-1] ^= 1
					writePrivate(t, paths[0], b)
				}
			case "unsafe_node":
				if os.Mkdir(filepath.Join(index, "unexpected-directory"), 0700) != nil {
					t.Fatal("fixture")
				}
			}
			before := len(readAudit(t, a.dir))
			err := a.note(Context{}, firstTestHits("{{EMAIL_after_damage}}", ""), 1, false)
			if err != errFirst {
				t.Fatalf("damage did not produce static diagnostic: %v", err)
			}
			if len(readAudit(t, a.dir)) != before+1 {
				t.Fatal("notification damage stopped durable audit")
			}
			if len(firstTestEvents(t, inbox)) != 1 {
				t.Fatal("corrupt index sent an unproved first-hit event")
			}
		})
	}
}

func TestFirstBoundedBucketAndHistory(t *testing.T) {
	t.Run("bucket", func(t *testing.T) {
		a, inbox := firstTestStore(t)
		target := firstDigest("{{EMAIL_capacity}}")
		err := a.withLock(func(_ *os.File) error {
			i, err := prepareFirst(a.dir, make(map[string]firstVerification))
			if err != nil {
				return err
			}
			batch := map[[32]byte]struct{}{}
			for n := 1; n <= firstBucketBytes/32; n++ {
				var d [32]byte
				d[0] = target[0]
				binary.BigEndian.PutUint64(d[24:], uint64(n))
				batch[d] = struct{}{}
			}
			if _, err = i.add(batch); err != nil {
				return err
			}
			return i.commit(map[string]firstStamp{}, true)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = a.note(Context{}, firstTestHits("{{EMAIL_capacity}}", ""), 1, false); err != errFirst {
			t.Fatal("full bucket was silently reset")
		}
		if len(firstTestEvents(t, inbox)) != 0 || len(readAudit(t, a.dir)) != 1 {
			t.Fatal("capacity failure changed audit/notification behavior")
		}
	})
	t.Run("history", func(t *testing.T) {
		a, _ := firstTestStore(t)
		err := a.withLock(func(_ *os.File) error {
			path := filepath.Join(a.dir, a.now().Local().Format(auditDay)+".jsonl")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if err = f.Truncate(firstHistoryMax + 1); err != nil {
				f.Close()
				return err
			}
			f.Close()
			if err = privatefile.Protect(path, 0600); err != nil {
				return err
			}
			_, err = prepareFirst(a.dir, make(map[string]firstVerification))
			if err != errFirst {
				return fmt.Errorf("oversized history was accepted")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestFirstInvalidHistoryCannotInventNewValues(t *testing.T) {
	for _, data := range []string{"{\"values\":[]}", "{broken}\n", "{\"values\":[{\"p\":\"\"}]}\n"} {
		t.Run(fmt.Sprintf("%d", len(data)), func(t *testing.T) {
			a, inbox := firstTestStore(t)
			if err := os.Mkdir(a.dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := privatefile.Protect(a.dir, 0700); err != nil {
				t.Fatal(err)
			}
			writePrivate(t, filepath.Join(a.dir, a.now().Local().Format(auditDay)+".jsonl"), []byte(data))
			err := a.note(Context{}, firstTestHits("{{EMAIL_not_proved}}", ""), 1, false)
			if err == nil || len(firstTestEvents(t, inbox)) != 0 {
				t.Fatal("bad seed history produced a notification")
			}
		})
	}
}

func TestFirstEventContainsOnlyNormalizedCounts(t *testing.T) {
	a, inbox := firstTestStore(t)
	var hits auditHits
	hits.add(redact.Hit{Placeholder: "{{private_placeholder}}", Kind: "private.kind@example.invalid", By: "known", Head: "private-head", Action: "block", Len: 10})
	if err := a.note(Context{Agent: "private.agent@example.invalid", Session: "private-session", What: "private-route"}, hits, 1, true); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(inbox, "*.json"))
	if len(files) != 1 {
		t.Fatal("missing event")
	}
	raw, _ := os.ReadFile(files[0])
	for _, v := range []string{"private", "placeholder", "session", "head", "route", "record_key"} {
		if bytes.Contains(raw, []byte(v)) {
			t.Fatal("desktop payload contains non-count context")
		}
	}
	var top map[string]json.RawMessage
	json.Unmarshal(raw, &top)
	if len(top) != 2 {
		t.Fatal("unexpected envelope fields")
	}
	var payload map[string]json.RawMessage
	json.Unmarshal(top["event"], &payload)
	if len(payload) != 4 {
		t.Fatal("unexpected privacy event fields")
	}
	got := firstTestEvents(t, inbox)[0]
	if got.Agent != "other" || len(got.Counts) != 1 || got.Counts[0] != (firstCount{"OTHER", 1}) {
		t.Fatal("untrusted label was not normalized")
	}
}

func TestFirstOmittedHitsAreIndexedButNotAnnounced(t *testing.T) {
	a, inbox := firstTestStore(t)
	h := firstTestHits("{{EMAIL_partial}}", "")
	h.omitted = 1
	if err := a.note(Context{}, h, 2, false); err != nil {
		t.Fatal(err)
	}
	firstTestNote(t, a, "{{EMAIL_partial}}", "")
	if len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("partial record produced complete-looking first counts")
	}
}

func firstTestEngineInbox(t *testing.T, s map[string]any) (*Engine, string) {
	t.Helper()
	e := testEngine(t, s)
	root := filepath.Dir(e.config.Settings)
	if err := privatefile.Protect(root, 0700); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(root, "desktop")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	c := e.config
	c.DesktopInbox = inbox
	enabled, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = enabled.Close() })
	return enabled, inbox
}

func TestFirstRealMaskWarnAndBlockSemantics(t *testing.T) {
	e, inbox := firstTestEngineInbox(t, map[string]any{"redactPersonal": true, "redactActions": map[string]string{"EMAIL": "warn"}})
	body := []byte(`{"messages":[{"role":"user","content":"warning.person@acme.io"}]}`)
	r, err := e.MaskWithContext(body, "", Context{Agent: "codex"})
	if err != nil || r.AuditError != "" || !bytes.Equal(r.Body, body) || len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("warn altered content or sent a masking alert")
	}
	settings := []byte(`{"redactPersonal":true,"redactModelOff":true,"redactActions":{"EMAIL":"block"}}`)
	writePrivate(t, e.config.Settings, settings)
	r, err = e.Mask(body, "")
	if err != nil || !r.Blocked || len(r.Body) != 0 || len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("prior warn was treated as a first block")
	}
	fresh := []byte(`{"messages":[{"role":"user","content":"blocked.person@acme.io"}]}`)
	r, err = e.MaskWithContext(fresh, "", Context{Agent: "codex"})
	if err != nil || r.AuditError != "" || !r.Blocked || len(r.Body) != 0 || len(firstTestEvents(t, inbox)) != 1 {
		t.Fatalf("first block behavior failed: %v / %s", err, r.AuditError)
	}
	writePrivate(t, e.config.Settings, []byte(`{"redactPersonal":true,"redactModelOff":true}`))
	fresh = []byte(`{"messages":[{"content":"masked.person@acme.io"}]}`)
	r, err = e.Mask(fresh, "")
	if err != nil || r.AuditError != "" || bytes.Contains(r.Body, []byte("masked.person@acme.io")) || len(firstTestEvents(t, inbox)) != 2 {
		t.Fatal("first ordinary mask did not preserve body/notification semantics")
	}
}

func TestFirstQueueFailureDoesNotChangeMaskedBodyOrReplay(t *testing.T) {
	e, inbox := firstTestEngineInbox(t, map[string]any{"redactPersonal": true})
	// A non-file child is rejected before publication on every platform.
	if err := os.Mkdir(filepath.Join(inbox, "unsafe-child"), 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"input":"queue.failure@acme.io"}`)
	r, err := e.Mask(body, "")
	if err != nil || r.Count == 0 || r.AuditError != errFirst.Error() || bytes.Contains(r.Body, []byte("queue.failure@acme.io")) {
		t.Fatal("queue failure changed privacy outcome")
	}
	if err = os.Remove(filepath.Join(inbox, "unsafe-child")); err != nil {
		t.Fatal(err)
	}
	again, err := e.Mask(body, "")
	if err != nil || !bytes.Equal(r.Body, again.Body) || len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("queue failure replayed a committed first hit")
	}
}

func TestFirstAuditWriteFailureNeverPublishes(t *testing.T) {
	e, inbox := firstTestEngineInbox(t, map[string]any{"redactPersonal": true})
	if err := os.Mkdir(e.audit.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(e.audit.dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.audit.dir, time.Now().Local().Format(auditDay)+".jsonl")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"input":"write.failure@acme.io"}`)
	r, err := e.Mask(body, "")
	if err != nil || r.Count == 0 || r.AuditError == "" || bytes.Contains(r.Body, []byte("write.failure@acme.io")) || len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("failed audit write changed mask or published")
	}
}

func TestFirstIndexSymlinkDoesNotFollowTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	} // Creation requires unrelated administrator privileges.
	a, inbox := firstTestStore(t)
	target := t.TempDir()
	if err := os.Symlink(target, firstIndexPath(a.dir)); err != nil {
		t.Fatal(err)
	}
	if err := a.note(Context{}, sampleHits(), 1, false); err != errFirst {
		t.Fatal("index symlink was accepted")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink target was touched")
	}
	if len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("unsafe index published")
	}
}

func TestFirstProcessWorker(t *testing.T) {
	root := os.Getenv("ZCALLD_FIRST_TEST_ROOT")
	if root == "" {
		return
	}
	start := filepath.Join(root, "start")
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("fixture start timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a := newAuditStore(filepath.Join(root, "history"))
	a.desktopInbox = filepath.Join(root, "inbox")
	a.lockWait = 2 * time.Second
	defer a.close()
	if err := a.note(Context{Agent: "codex"}, firstTestHits("{{EMAIL_shared_process}}", ""), 1, false); err != nil {
		t.Fatal(err)
	}
}

func TestFirstCrossProcessDeduplication(t *testing.T) {
	a, inbox := firstTestStore(t)
	root := filepath.Dir(a.dir)
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for n := range commands {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFirstProcessWorker$", "-test.count=1")
		cmd.Env = append(os.Environ(), "ZCALLD_FIRST_TEST_ROOT="+root)
		cmd.Stdout = &outputs[n]
		cmd.Stderr = &outputs[n]
		commands[n] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	writePrivate(t, filepath.Join(root, "start"), []byte("go"))
	for n, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child failed: %v: %s", err, outputs[n].String())
		}
	}
	firstTestNote(t, a, "{{EMAIL_shared_process}}", "")
	events := firstTestEvents(t, inbox)
	if len(events) != 1 || firstTestCount(events) != 1 {
		t.Fatal("multiple workers announced the same value")
	}
}

func TestFirstSeedRejectsUnexpectedPrivateLabelsWithoutLeaking(t *testing.T) {
	a, inbox := firstTestStore(t)
	long := strings.Repeat("x", 513)
	err := a.withLock(func(_ *os.File) error {
		path := filepath.Join(a.dir, a.now().Local().Format(auditDay)+".jsonl")
		raw, _ := json.Marshal(map[string]any{"values": []any{map[string]any{"p": long}}})
		writePrivate(t, path, append(raw, '\n'))
		_, err := prepareFirst(a.dir, make(map[string]firstVerification))
		if err != errFirst {
			return fmt.Errorf("oversized placeholder was accepted")
		}
		return nil
	})
	if err != nil || len(firstTestEvents(t, inbox)) != 0 {
		t.Fatal("invalid bounded seed failed unsafe")
	}
}
