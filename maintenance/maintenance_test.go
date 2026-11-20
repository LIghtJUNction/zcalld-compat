package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const account = "builtin:codex#0123456789abcdef0123"

var moment = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

type mockHost struct {
	inspect      func(context.Context, string) (Snapshot, error)
	warm         func(context.Context, string, []string) (ActionResult, error)
	consume      func(context.Context, string, ConsumeRequest) (ActionResult, error)
	reads, warms int
	spends       []ConsumeRequest
}

func (h *mockHost) Inspect(c context.Context, a string) (Snapshot, error) {
	h.reads++
	if h.inspect != nil {
		return h.inspect(c, a)
	}
	return Snapshot{}, errors.New("not configured")
}
func (h *mockHost) Warmup(c context.Context, a string, w []string) (ActionResult, error) {
	h.warms++
	if h.warm != nil {
		return h.warm(c, a, w)
	}
	return ActionResult{Status: "completed"}, nil
}
func (h *mockHost) Consume(c context.Context, a string, p ConsumeRequest) (ActionResult, error) {
	h.spends = append(h.spends, p)
	if h.consume != nil {
		return h.consume(c, a, p)
	}
	return ActionResult{Status: "completed", Code: "reset"}, nil
}
func instant(t time.Time) *time.Time { return &t }
func snap(now time.Time) Snapshot {
	return Snapshot{Account: account, Enabled: true, Fresh: true, ObservedAt: now, UsageKnown: true, ModelsKnown: true, CardsComplete: true, Windows: []Window{{Name: "weekly", SpanSeconds: 7 * 86400, Used: 0, ResetsAt: instant(now.Add(7 * 24 * time.Hour))}}}
}
func newStore(t *testing.T, weekly, reset bool) Store {
	t.Helper()
	s := Store{Directory: filepath.Join(t.TempDir(), "state")}
	settings := `{"codexWarmup":"week","codexAutoReset":["test@example.invalid"],"codexAutoResetOnly":{"test@example.invalid":"expiring"}}`
	plan, err := PlanImport([]byte(settings), []byte("{}\n"), []AccountLink{{"test@example.invalid", account, true}})
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Accounts[account]
	p.WeeklyWarmup = weekly
	p.ExpiringReset = reset
	plan.Accounts[account] = p
	plan.Hash = planHash(plan)
	if err = s.ApplyPlan(t.Context(), plan, plan.Hash, true, moment.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return s
}
func run(t *testing.T, s Store, h *mockHost, now time.Time) Report {
	t.Helper()
	r, e := (Engine{Store: s, Host: h, Now: func() time.Time { return now }}).Run(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestDisabledHasNoFilesystemOrNetworkSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created")
	s := Store{Directory: path}
	h := new(mockHost)
	r, e := (Engine{Store: s, Host: h}).Run(t.Context())
	if e != nil || r.Status != "disabled" || h.reads != 0 {
		t.Fatalf("unexpected disabled result: %+v %v", r, e)
	}
	status, e := s.Status()
	if e != nil || status.Enabled || !status.LocalOnly {
		t.Fatal(status, e)
	}
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("status or disabled run created state")
	}
}
func TestImportRetainsLedgerAndDoesNotAdoptNewAccounts(t *testing.T) {
	raw := []byte("{\n  \"test@example.invalid\": {\"weekly\": {\"used\":17,\"resetsAt\":\"2026-11-25T10:00:00Z\",\"warmed\":\"2026-11-18T10:00:00Z\",\"idle\":2,\"retry\":\"2026-11-20T12:00:00Z\"}}\n}\n")
	cfg := []byte(`{"codexWarmup":"week","codexAutoReset":["test@example.invalid","unknown@example.invalid"],"codexAutoResetOnly":{"test@example.invalid":"expiring"},"codexNoCredits":["test@example.invalid"]}`)
	p, e := PlanImport(cfg, raw, []AccountLink{{" TEST@example.invalid ", account, true}})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Accounts) != 1 || p.UnmappedAccounts != 1 || !bytes.Equal(p.LegacyWarmup, raw) || !p.Accounts[account].ExpiringReset {
		t.Fatal("policy mapping changed")
	}
	s := Store{Directory: filepath.Join(t.TempDir(), "state")}
	if e = s.ApplyPlan(t.Context(), p, p.Hash, true, moment); e != nil {
		t.Fatal(e)
	}
	d, e := s.Read()
	if e != nil {
		t.Fatal(e)
	}
	if d.Ledger[account].Windows["weekly"].Used != 17 || d.Ledger[account].Windows["weekly"].Idle != 2 || !bytes.Equal(d.LegacyWarmup, raw) {
		t.Fatal("legacy ledger lost")
	}
	tx, e := s.acquire(t.Context(), false)
	if e != nil {
		t.Fatal(e)
	}
	tx.doc.Ledger[account].LastResult = "new-runtime-record"
	e = tx.save()
	tx.close()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ApplyPlan(t.Context(), p, p.Hash, true, moment.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	d, _ = s.Read()
	if d.Ledger[account].LastResult != "new-runtime-record" || !d.Policy.EnabledAt.Equal(moment) {
		t.Fatal("reimport overwrote runtime state")
	}
	p.Accounts[account] = AccountPolicy{ResetLeadMinutes: 30}
	if e = s.ApplyPlan(t.Context(), p, p.Hash, true, moment); e == nil {
		t.Fatal("mutated plan accepted")
	}
}
func TestInvalidSchedulesAndAmbiguousSourceAreRejected(t *testing.T) {
	for _, cfg := range []string{`{"codexWarmup":"unknown"}`, `{"codexWarmup":"week","codexWarmAts":["26:00"]}`, `{"codexAutoResetOnly":{"test@example.invalid":"unknown"}}`} {
		if _, e := PlanImport([]byte(cfg), nil, []AccountLink{{"test@example.invalid", account, true}}); e == nil {
			t.Fatal("unsupported policy accepted")
		}
	}
	if _, e := PlanImport([]byte(`{"codexWarmup":"week"}`), nil, []AccountLink{{"test@example.invalid", account, true}, {"TEST@example.invalid", "builtin:codex#aaaaaaaaaaaaaaaaaaaa", true}}); e == nil {
		t.Fatal("ambiguous identity accepted")
	}
}
func TestImportedDurationLedgerReconcilesWithoutRepeatingWarmup(t *testing.T) {
	s := newStore(t, true, false)
	tx, e := s.acquire(t.Context(), false)
	if e != nil {
		t.Fatal(e)
	}
	tx.doc.Ledger[account].Windows["7 days"] = WarmWindow{Used: 17, ResetsAt: moment.Add(5 * 24 * time.Hour), Warmed: moment.Add(-time.Hour)}
	if e = tx.save(); e != nil {
		t.Fatal(e)
	}
	tx.close()
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		q := snap(moment)
		q.Windows[0].Name = "secondary"
		q.Windows[0].Used = 17
		q.Windows[0].ResetsAt = instant(moment.Add(5 * 24 * time.Hour))
		return q, nil
	}}
	run(t, s, h, moment)
	if h.warms != 0 {
		t.Fatal("imported duration name treated as a new window")
	}
	d, _ := s.Read()
	if d.Ledger[account].Windows["secondary"].Used != 17 {
		t.Fatal("source ledger was not transferred")
	}
	if _, old := d.Ledger[account].Windows["7 days"]; old {
		t.Fatal("ambiguous duplicate ledger remained")
	}
}
func TestKnownNoSpendDoesNotMarkResetCardConsumed(t *testing.T) {
	s := newStore(t, false, true)
	expiry := moment.Add(10 * time.Minute)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		q := snap(moment)
		q.Windows[0].Used = 20
		q.Cards = []ResetCard{{"same-card", &expiry}}
		return q, nil
	}, consume: func(context.Context, string, ConsumeRequest) (ActionResult, error) {
		return ActionResult{Status: "completed", Code: "nothing_to_reset"}, nil
	}}
	run(t, s, h, moment)
	d, _ := s.Read()
	if len(d.Ledger[account].CompletedCards) != 0 || d.Ledger[account].Reset != nil || d.Ledger[account].LastResult != "reset_not_needed" {
		t.Fatal("a known no-spend was misreported")
	}
}
func TestNoCreditsUnknownAndHeldNeverGenerate(t *testing.T) {
	for _, which := range []string{"unknown", "held", "missing-model", "disabled-account"} {
		t.Run(which, func(t *testing.T) {
			s := newStore(t, true, false)
			h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
				q := snap(moment)
				q.NoCredits = true
				switch which {
				case "unknown":
					q.UsageKnown = false
				case "held":
					q.Held = true
				case "missing-model":
					q.ModelsKnown = false
				case "disabled-account":
					q.Enabled = false
				}
				return q, nil
			}}
			run(t, s, h, moment)
			if h.warms != 0 {
				t.Fatal("unsafe generation")
			}
		})
	}
}
func TestWarmupUnknownOutcomeDoesNotReplayUntilWindowObservedActive(t *testing.T) {
	s := newStore(t, true, false)
	now := moment
	active := false
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		q := snap(now)
		if active {
			q.Windows[0].Used = 2
			q.Windows[0].ResetsAt = instant(now.Add(6 * 24 * time.Hour))
		}
		return q, nil
	}, warm: func(context.Context, string, []string) (ActionResult, error) {
		d, e := s.Read()
		if e != nil || d.Ledger[account].Warm.Phase != "sending" {
			t.Fatal("attempt not durably recorded")
		}
		return ActionResult{}, errors.New("unknown transport result")
	}}
	run(t, s, h, now)
	now = now.Add(6 * time.Minute)
	run(t, s, h, now)
	if h.warms != 1 {
		t.Fatal("ambiguous warmup repeated")
	}
	active = true
	now = now.Add(6 * time.Minute)
	run(t, s, h, now)
	if h.warms != 1 {
		t.Fatal("active window regenerated")
	}
	d, _ := s.Read()
	if d.Ledger[account].Warm != nil || d.Ledger[account].LastResult != "window_observed_active" {
		t.Fatal("fresh window was not reconciled")
	}
}
func TestCrashBetweenIntentAndRPCDoesNotLoseGuard(t *testing.T) {
	s := newStore(t, true, false)
	s.afterSave = func(d Document) error {
		if d.Ledger[account].Warm != nil && d.Ledger[account].Warm.Phase == "sending" {
			return errors.New("simulated process exit after fsync")
		}
		return nil
	}
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) { return snap(moment), nil }}
	_, e := (Engine{Store: s, Host: h, Now: func() time.Time { return moment }}).Run(t.Context())
	if e == nil || h.warms != 0 {
		t.Fatal("crash hook did not stop before RPC")
	}
	s.afterSave = nil
	run(t, s, h, moment.Add(6*time.Minute))
	if h.warms != 0 {
		t.Fatal("uncertain persisted authority replayed")
	}
}
func TestResetRetriesUseExactDurableIDAndNeverSpendNextCard(t *testing.T) {
	s := newStore(t, false, true)
	now := moment
	expiry := moment.Add(20 * time.Minute)
	second := moment.Add(25 * time.Minute)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		q := snap(now)
		q.Windows[0].Used = 25
		q.Cards = []ResetCard{{"card-one", &expiry}, {"card-two", &second}}
		return q, nil
	}, consume: func(context.Context, string, ConsumeRequest) (ActionResult, error) {
		return ActionResult{}, errors.New("response lost after POST")
	}}
	run(t, s, h, now)
	if len(h.spends) != 1 {
		t.Fatal("reset not attempted")
	}
	first := h.spends[0]
	if !redeemPattern.MatchString(first.RedeemRequestID) {
		t.Fatal("invalid durable request id")
	}
	now = now.Add(time.Minute)
	h.consume = nil
	run(t, s, h, now)
	if len(h.spends) != 2 || h.spends[1].CreditID != first.CreditID || h.spends[1].RedeemRequestID != first.RedeemRequestID {
		t.Fatal("retry changed card or idempotency key")
	}
	d, _ := s.Read()
	if _, ok := d.Ledger[account].CompletedCards["card-one"]; !ok {
		t.Fatal("successful spend not recorded")
	}
}
func TestResetSecondLookCannotSubstituteAnotherCard(t *testing.T) {
	s := newStore(t, false, true)
	expiry := moment.Add(10 * time.Minute)
	reads := 0
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		reads++
		q := snap(moment)
		q.Windows[0].Used = 30
		id := "first"
		if reads > 1 {
			id = "different"
		}
		q.Cards = []ResetCard{{id, &expiry}}
		return q, nil
	}}
	run(t, s, h, moment)
	if len(h.spends) != 0 {
		t.Fatal("substituted card was consumed")
	}
	d, _ := s.Read()
	if d.Ledger[account].Reset.CreditID != "first" {
		t.Fatal("original card identity lost")
	}
}
func TestCredentialChangeKeepsOriginalResetIDAndUncertainty(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "previous-send-unknown"}[uncertain], func(t *testing.T) {
			s := newStore(t, false, true)
			now := moment
			expires := moment.Add(25 * time.Minute)
			attempts := 0
			h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
				q := snap(now)
				q.Windows[0].Used = 40
				q.Cards = []ResetCard{{"original-card", &expires}}
				return q, nil
			}, consume: func(context.Context, string, ConsumeRequest) (ActionResult, error) {
				attempts++
				if uncertain && attempts == 1 {
					return ActionResult{}, errors.New("lost after send")
				}
				return ActionResult{Status: "not_sent", Code: "credentials_changed"}, nil
			}}
			run(t, s, h, now)
			original := h.spends[0]
			if uncertain {
				now = now.Add(time.Minute)
				run(t, s, h, now)
			}
			d, e := s.Read()
			if e != nil {
				t.Fatal(e)
			}
			intent := d.Ledger[account].Reset
			phase := "prepared"
			if uncertain {
				phase = "uncertain"
			}
			if intent == nil || intent.Phase != phase || intent.CreditID != original.CreditID || intent.RedeemRequestID != original.RedeemRequestID || len(d.Ledger[account].CompletedCards) != 0 {
				t.Fatal("a no-send credential change discarded authority or prior uncertainty")
			}
			now = now.Add(6 * time.Minute)
			h.consume = nil
			run(t, s, h, now)
			retry := h.spends[len(h.spends)-1]
			if retry.CreditID != original.CreditID || retry.RedeemRequestID != original.RedeemRequestID {
				t.Fatal("credential restoration authorized a different card or request ID")
			}
		})
	}
}
func TestDeadlineIsSharedByTheWholeRun(t *testing.T) {
	s := newStore(t, true, false)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	h := &mockHost{inspect: func(c context.Context, _ string) (Snapshot, error) { <-c.Done(); return Snapshot{}, c.Err() }}
	start := time.Now()
	r, e := (Engine{Store: s, Host: h, Now: func() time.Time { return moment }}).Run(ctx)
	if e != nil || r.Status != "deadline" || time.Since(start) > time.Second || h.warms != 0 {
		t.Fatal(r, e)
	}
}
func TestSlowInspectionUsesReceiptTimeForFreshness(t *testing.T) {
	s := newStore(t, true, false)
	now := moment
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) { now = now.Add(7 * time.Second); return snap(now), nil }}
	r, e := (Engine{Store: s, Host: h, Now: func() time.Time { return now }}).Run(t.Context())
	if e != nil || r.Status != "complete" || h.warms != 1 {
		t.Fatal("fresh completed inspection was mistaken for future data", r, e)
	}
}
func TestProcessLockChild(t *testing.T) {
	if os.Getenv("ZCALLD_MAINTENANCE_LOCK_CHILD") != "1" {
		return
	}
	s := Store{Directory: os.Getenv("ZCALLD_MAINTENANCE_TEST_STATE")}
	tx, e := s.acquire(context.Background(), false)
	if tx != nil {
		tx.close()
	}
	if !errors.Is(e, ErrBusy) {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestDifferentProcessCannotEnterSameLedger(t *testing.T) {
	s := newStore(t, true, false)
	tx, e := s.acquire(t.Context(), false)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessLockChild$")
	cmd.Env = append(os.Environ(), "ZCALLD_MAINTENANCE_LOCK_CHILD=1", "ZCALLD_MAINTENANCE_TEST_STATE="+s.Directory)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("cross-process exclusion failed: %s %v", b, e)
	}
}
func TestUnsafeStateNodeAndReplacedLockAreRejected(t *testing.T) {
	s := newStore(t, true, false)
	tx, e := s.acquire(t.Context(), false)
	if e != nil {
		t.Fatal(e)
	}
	lock := filepath.Join(s.Directory, ".run.lock")
	if e = os.Rename(lock, lock+".old"); e != nil {
		tx.close()
		if runtime.GOOS == "windows" {
			t.Skip("Windows prevents renaming the open lock")
		}
		t.Fatal(e)
	}
	if e = os.WriteFile(lock, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = tx.save(); !errors.Is(e, ErrUnsafe) {
		t.Fatal("replaced lock was trusted", e)
	}
	tx.close()
	target := filepath.Join(s.Directory, "outside.json")
	if e = os.Rename(filepath.Join(s.Directory, "state.json"), target); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(target, filepath.Join(s.Directory, "state.json")); e != nil {
		t.Skip("symlink creation unavailable")
	}
	if _, e = s.Read(); !errors.Is(e, ErrUnsafe) {
		t.Fatal("symlink state was read")
	}
}
func TestHostFailureDetailsDoNotEnterReport(t *testing.T) {
	s := newStore(t, true, false)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) { return Snapshot{}, errors.New("canary-secret-token") }}
	r := run(t, s, h, moment)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "canary") {
		t.Fatal("host error leaked")
	}
}
