package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func policyStore(t *testing.T, settings string, now time.Time) Store {
	t.Helper()
	p, e := PlanImport([]byte(settings), nil, []AccountLink{{"test@example.invalid", account, true}})
	if e != nil {
		t.Fatal(e)
	}
	s := Store{Directory: filepath.Join(t.TempDir(), "state")}
	if e = s.ApplyPlan(t.Context(), p, p.Hash, true, now.Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	return s
}

func TestFullPolicyModesClocksAndOverrides(t *testing.T) {
	for _, mode := range []string{"off", "week", "all"} {
		t.Run(mode, func(t *testing.T) {
			p, e := PlanImport([]byte(`{"codexWarmup":"`+mode+`","codexWarmAt":"06:00","codexWarmAts":["10:05:30","06:00","10:05"],"codexWarmAtOf":{"test@example.invalid":" 08:00 "},"codexAutoReset":["test@example.invalid"]}`), nil, []AccountLink{{"test@example.invalid", account, true}})
			if e != nil {
				t.Fatal(e)
			}
			a := p.Accounts[account]
			if a.WeeklyWarmup != (mode != "off") || a.FiveHourWarmup != (mode == "all") || len(a.WarmAts) != 1 || a.WarmAts[0] != "08:00" || !a.WeeklyReset || !a.ExpiringReset {
				t.Fatal(a)
			}
		})
	}
	for _, which := range []string{"week", "expiring"} {
		p, e := PlanImport([]byte(`{"codexAutoReset":["test@example.invalid"],"codexAutoResetOnly":{"TEST@example.invalid":"`+which+`"}}`), nil, []AccountLink{{"test@example.invalid", account, true}})
		if e != nil {
			t.Fatal(e)
		}
		a := p.Accounts[account]
		if a.WeeklyReset != (which == "week") || a.ExpiringReset != (which == "expiring") {
			t.Fatal(a)
		}
	}
	for _, settings := range []string{
		`{"codexWarmup":"week","codexWarmAt":"06:00","codexWarmAts":[]}`,
		`{"codexWarmup":"week","codexWarmAts":["06:00"],"codexWarmAtOf":{"test@example.invalid":"off"}}`,
	} {
		p, e := PlanImport([]byte(settings), nil, []AccountLink{{"test@example.invalid", account, true}})
		if e != nil || len(p.Accounts[account].WarmAts) != 0 {
			t.Fatal("disabled clock was resurrected", p, e)
		}
	}
}

func TestFiveHourWarmupIsExplicitAndClockHoldsBothWindows(t *testing.T) {
	for _, mode := range []string{"week", "all"} {
		s := policyStore(t, `{"codexWarmup":"`+mode+`"}`, moment)
		h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
			q := snap(moment)
			q.Windows[0].Used = 20
			q.Windows[0].ResetsAt = instant(moment.Add(5 * 24 * time.Hour))
			q.Windows = append(q.Windows, Window{Name: "primary", SpanSeconds: 18000, ResetsAt: instant(moment.Add(5 * time.Hour))})
			return q, nil
		}}
		run(t, s, h, moment)
		expected := 0
		if mode == "all" {
			expected = 1
		}
		if h.warms != expected {
			t.Fatal(mode, h.warms)
		}
	}
	now := moment.Add(-2 * time.Hour)
	s := policyStore(t, `{"codexWarmup":"all","codexWarmAts":["10:00"]}`, now)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
		q := snap(now)
		q.Windows = append(q.Windows, Window{Name: "primary", SpanSeconds: 18000, ResetsAt: instant(now.Add(5 * time.Hour))})
		return q, nil
	}}
	run(t, s, h, now)
	if h.warms != 0 {
		t.Fatal("reset warmup would overlap the requested daily start")
	}
	now = moment.Add(5 * time.Minute)
	run(t, s, h, now)
	if h.warms != 1 {
		t.Fatal("clock did not start the windows")
	}
}

func TestLocalClockCatchupAndOncePerDayLedger(t *testing.T) {
	zone := time.FixedZone("fixture-local", 8*3600)
	for _, minutes := range []int{30, 61} {
		t.Run(time.Duration(minutes).String(), func(t *testing.T) {
			now := time.Date(2026, 11, 20, 9, minutes, 0, 0, zone)
			s := policyStore(t, `{"codexWarmup":"off","codexWarmAts":["09:00"]}`, now)
			h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
				q := snap(now)
				q.Windows = []Window{{Name: "primary", SpanSeconds: 18000, ResetsAt: instant(now.Add(5 * time.Hour))}}
				return q, nil
			}}
			run(t, s, h, now)
			want := 0
			if minutes == 30 {
				want = 1
			}
			if h.warms != want {
				t.Fatal("wrong local/catchup semantics", h.warms)
			}
			now = now.Add(6 * time.Minute)
			run(t, s, h, now)
			if h.warms != want {
				t.Fatal("same local clock repeated")
			}
			if want == 1 {
				d, e := s.Read()
				if e != nil || d.Ledger[account].Windows["primary"].Daily != "2026-11-20 09:00" {
					t.Fatal("missing exact local daily marker", e)
				}
			}
		})
	}
	w := Window{Name: "primary", SpanSeconds: 18000}
	now := time.Date(2026, 11, 20, 9, 10, 0, 0, zone)
	if _, due := dailyDue([]string{"09:00"}, "2026-11-20", w, now); due {
		t.Fatal("legacy whole-day marker repeated")
	}
}

func TestWeeklyHistoryImportPreservesPrimaryFallbackAndUnknown(t *testing.T) {
	settings := []byte(`{"codexAutoReset":["test@example.invalid"],"codexAutoResetOnly":{"test@example.invalid":"week"}}`)
	links := []AccountLink{{"test@example.invalid", account, true}}
	primary, _ := json.Marshal(map[string]any{"test@example.invalid": map[string]any{"until": moment.Add(2 * 24 * time.Hour), "at": moment.Add(-time.Hour), "outcome": map[string]any{"code": "reset", "windows": 2}}})
	audit, _ := json.Marshal([]map[string]any{{"user": "test@example.invalid", "at": moment.Add(-24 * time.Hour), "why": "week"}, {"user": "test@example.invalid", "at": moment, "why": "expiring"}})
	for _, test := range []struct {
		name           string
		primary, audit []byte
		until          time.Time
		unknown        bool
	}{
		{"primary", primary, audit, moment.Add(2 * 24 * time.Hour), false},
		{"audit", []byte("broken"), audit, moment.Add(6 * 24 * time.Hour), false},
		{"audit-without-primary", nil, audit, moment.Add(6 * 24 * time.Hour), false},
		{"both-missing", nil, nil, time.Time{}, false},
		{"unknown", []byte("broken"), []byte("broken"), moment.Add(7 * 24 * time.Hour), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, e := PlanImportLedgers(settings, nil, test.primary, test.audit, links)
			if e != nil {
				t.Fatal(e)
			}
			if p.WeeklyHistoryUnknown != test.unknown {
				t.Fatal("history proof changed")
			}
			s := Store{Directory: filepath.Join(t.TempDir(), "state")}
			if e = s.ApplyPlan(t.Context(), p, p.Hash, true, moment); e != nil {
				t.Fatal(e)
			}
			d, e := s.Read()
			if e != nil || !d.Ledger[account].Weekly.Until.Equal(test.until) {
				t.Fatal("weekly guard lost", e)
			}
		})
	}
}

func weeklySnapshot(now time.Time) Snapshot {
	q := snap(now)
	q.Windows[0].Used = 100
	q.Windows[0].ResetsAt = instant(moment.Add(2 * 24 * time.Hour))
	q.OnCredits = true
	q.Cards = []ResetCard{{ID: "nonexpiring-card"}}
	return q
}
func TestTimerNeverSpendsWeeklyAndRequestResetIsOncePerWeek(t *testing.T) {
	s := policyStore(t, `{"codexAutoReset":["test@example.invalid"],"codexAutoResetOnly":{"test@example.invalid":"week"}}`, moment)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) { return weeklySnapshot(moment), nil }}
	run(t, s, h, moment)
	if h.reads != 0 || len(h.spends) != 0 {
		t.Fatal("timer executed request-only weekly policy")
	}
	engine := Engine{Store: s, Host: h, Now: func() time.Time { return moment }}
	reply, e := engine.RunWeekly(t.Context(), account, WeeklyBeforeCredits)
	if e != nil || reply.Status != "completed" || reply.Code != "reset" || len(h.spends) != 1 {
		t.Fatal(reply, e)
	}
	request := h.spends[0]
	if request.Purpose != "week" || request.Trigger != WeeklyBeforeCredits || !request.ExpectedExpiresAt.IsZero() || request.RedeemRequestID == "" {
		t.Fatal(request)
	}
	reads := h.reads
	reply, e = engine.RunWeekly(t.Context(), account, WeeklyRoutesExhausted)
	if e != nil || reply.Code != "weekly_already_used" || h.reads != reads || len(h.spends) != 1 {
		t.Fatal("spent-week fast path failed", reply, e)
	}
}
func TestWeeklyBeforeCreditsAndExhaustedPredicatesDiffer(t *testing.T) {
	for _, test := range []struct {
		name                               string
		trigger                            WeeklyTrigger
		noCredits, onCredits, full, future bool
		spends                             int
	}{
		{"paid", WeeklyBeforeCredits, false, true, true, true, 1},
		{"no-credits-before", WeeklyBeforeCredits, true, true, true, true, 0},
		{"not-on-credits", WeeklyBeforeCredits, false, false, true, true, 0},
		{"not-full", WeeklyBeforeCredits, false, true, false, true, 0},
		{"past-reset", WeeklyRoutesExhausted, true, false, true, false, 0},
		{"exhausted-after-alternatives", WeeklyRoutesExhausted, true, false, true, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := policyStore(t, `{"codexAutoReset":["test@example.invalid"],"codexAutoResetOnly":{"test@example.invalid":"week"}}`, moment)
			h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) {
				q := weeklySnapshot(moment)
				q.NoCredits = test.noCredits
				q.OnCredits = test.onCredits
				if !test.full {
					q.Windows[0].Used = 90
				}
				if !test.future {
					q.Windows[0].ResetsAt = instant(moment.Add(-time.Second))
				}
				return q, nil
			}}
			_, e := (Engine{Store: s, Host: h, Now: func() time.Time { return moment }}).RunWeekly(t.Context(), account, test.trigger)
			if e != nil || len(h.spends) != test.spends {
				t.Fatal("wrong reset authority", len(h.spends), e)
			}
		})
	}
}
func TestWeeklyUnknownKeepsCardAndIDAcrossRetry(t *testing.T) {
	now := moment
	s := policyStore(t, `{"codexAutoReset":["test@example.invalid"]}`, now)
	h := &mockHost{inspect: func(context.Context, string) (Snapshot, error) { return weeklySnapshot(now), nil }, consume: func(context.Context, string, ConsumeRequest) (ActionResult, error) {
		return ActionResult{}, errors.New("unknown after send")
	}}
	engine := Engine{Store: s, Host: h, Now: func() time.Time { return now }}
	reply, e := engine.RunWeekly(t.Context(), account, WeeklyRoutesExhausted)
	if e != nil || reply.Status != "unknown" {
		t.Fatal(reply, e)
	}
	first := h.spends[0]
	now = now.Add(time.Minute)
	h.consume = nil
	reply, e = engine.RunWeekly(t.Context(), account, WeeklyRoutesExhausted)
	if e != nil || reply.Code != "reset" || len(h.spends) != 2 || h.spends[1].RedeemRequestID != first.RedeemRequestID || h.spends[1].CreditID != first.CreditID {
		t.Fatal("uncertain reset spent a new identity", reply, e)
	}
}
func TestPolicyUpdateRetainsLiveDailyAndWeeklyLedger(t *testing.T) {
	s := policyStore(t, `{"codexWarmup":"week"}`, moment)
	tx, e := s.acquire(t.Context(), false)
	if e != nil {
		t.Fatal(e)
	}
	tx.doc.Ledger[account].Weekly = WeeklyRecord{Until: moment.Add(3 * 24 * time.Hour), Code: "reset"}
	tx.doc.Ledger[account].Windows["primary"] = WarmWindow{Daily: "2026-11-20 10:00", Used: 12}
	if e = tx.save(); e != nil {
		t.Fatal(e)
	}
	tx.close()
	p, e := PlanImport([]byte(`{"codexWarmup":"all","codexWarmAts":["10:00"],"codexAutoReset":["test@example.invalid"]}`), nil, []AccountLink{{"test@example.invalid", account, true}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ApplyPlan(t.Context(), p, p.Hash, false, moment); e != nil {
		t.Fatal(e)
	}
	d, e := s.Read()
	if e != nil || d.Policy.Enabled || !d.Policy.Accounts[account].FiveHourWarmup || d.Ledger[account].Windows["primary"].Daily != "2026-11-20 10:00" || !d.Ledger[account].Weekly.Until.Equal(moment.Add(3*24*time.Hour)) {
		t.Fatal("policy update erased live protections", e)
	}
}
