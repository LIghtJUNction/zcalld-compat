package maintenance

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

var redeemPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

type Engine struct {
	Store Store
	Host  Host
	Now   func() time.Time
	// Observe receives only the fresh inspection already needed by maintenance.
	// Notification publishing is optional and cannot fail a maintenance action.
	Observe func(Snapshot) error
}

func (e Engine) clock() time.Time {
	if e.Now != nil {
		return e.Now().Round(0)
	}
	return time.Now().Round(0)
}

func (e Engine) Run(ctx context.Context) (Report, error) {
	result := Report{Status: "disabled", Results: []AccountResult{}}
	d, err := e.Store.Read()
	if err != nil {
		return result, err
	}
	if !d.Policy.Enabled {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultBudget)
	defer cancel()
	tx, err := e.Store.acquire(ctx, false)
	if errors.Is(err, ErrBusy) {
		result.Status = "busy"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer tx.close()
	if !tx.doc.Policy.Enabled {
		return result, nil
	}
	if e.Host == nil {
		return result, errors.New("maintenance host is unavailable")
	}
	result.Enabled = true
	result.Status = "complete"
	accounts := make([]string, 0, len(tx.doc.Policy.Accounts))
	for a := range tx.doc.Policy.Accounts {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		if err = ctx.Err(); err != nil {
			result.Status = "deadline"
			break
		}
		policy := tx.doc.Policy.Accounts[account]
		now := e.clock()
		ledger := tx.doc.Ledger[account]
		if ledger == nil {
			ledger = &AccountLedger{Windows: map[string]WarmWindow{}, CompletedCards: map[string]time.Time{}}
			tx.doc.Ledger[account] = ledger
		}
		if ledger.Windows == nil {
			ledger.Windows = map[string]WarmWindow{}
		}
		if ledger.CompletedCards == nil {
			ledger.CompletedCards = map[string]time.Time{}
		}
		warm := !now.Before(tx.doc.Policy.EnabledAt.Add(2*time.Minute)) && warmCheckDue(policy, ledger, now)
		reset := policy.ExpiringReset && !now.Before(tx.doc.Policy.EnabledAt.Add(3*time.Minute)) && !now.Before(ledger.NextResetCheck)
		if !warm && !reset {
			continue
		}
		snap, readErr := e.Host.Inspect(ctx, account)
		now = e.clock() // A completed inspection is timed against receipt, not its start.
		if readErr != nil || !freshSnapshot(snap, account, now) {
			if warm {
				ledger.LastWarmCheck = now
				ledger.NextWarmCheck = now.Add(time.Minute)
			}
			if reset {
				ledger.NextResetCheck = now.Add(time.Minute)
			}
			ledger.LastResult = "inspection_unavailable"
			if err = tx.save(); err != nil {
				return result, err
			}
			result.Results = append(result.Results, AccountResult{account, "inspect", ledger.LastResult})
			continue
		}
		if e.Observe != nil {
			_ = e.Observe(snap)
		}
		if policy.ExpiringReset && !now.Before(tx.doc.Policy.EnabledAt.Add(3*time.Minute)) && nextResetCheck(snap, policy, now).Before(ledger.NextResetCheck) {
			reset = true
		}
		if warm {
			ledger.LastWarmCheck = now
			ledger.NextWarmCheck = now.Add(5 * time.Minute)
		}
		if reset {
			ledger.NextResetCheck = nextResetCheck(snap, policy, now)
		}
		if err = tx.save(); err != nil {
			return result, err
		}
		if reset {
			acted, status, runErr := e.reset(ctx, tx, account, policy, ledger, snap, now)
			if runErr != nil {
				return result, runErr
			}
			if status != "" {
				result.Results = append(result.Results, AccountResult{account, "consume", status})
			}
			if acted {
				continue
			} // never combine a reset spend and a generation in one pass
		}
		if warm {
			status, runErr := e.warm(ctx, tx, account, policy, ledger, snap, now)
			if runErr != nil {
				return result, runErr
			}
			if status != "" {
				result.Results = append(result.Results, AccountResult{account, "warmup", status})
			}
		}
	}
	if ctx.Err() != nil {
		result.Status = "deadline"
	}
	return result, nil
}

func weekly(w Window) bool { return w.SpanSeconds >= 86400 && !w.Aside && w.Model == "" }
func asNow(w Window, now time.Time) Window {
	if w.ResetsAt != nil && !now.Before(*w.ResetsAt) {
		w.Used = 0
		w.ResetsAt = nil
	}
	return w
}
func idleWindow(w Window, now time.Time) bool {
	return w.Used == 0 && (w.ResetsAt == nil || w.ResetsAt.Sub(now) >= time.Duration(w.SpanSeconds)*time.Second-10*time.Minute)
}
func observed(w Window, p WarmWindow) WarmWindow {
	out := p
	out.Used = w.Used
	out.ResetsAt = time.Time{}
	if w.ResetsAt != nil {
		out.ResetsAt = *w.ResetsAt
	}
	out.Absent = false
	return out
}
func needsWarm(p WarmWindow, seen bool, w Window, now time.Time) bool {
	if !seen {
		return idleWindow(w, now)
	}
	if p.Pending {
		return false
	} // old ambiguous attempts are never blindly replayed
	if p.Idle > 0 && idleWindow(w, now) {
		return !now.Before(p.Retry)
	}
	if p.Used-w.Used >= 1 {
		return true
	}
	if !p.ResetsAt.IsZero() && idleWindow(w, now) {
		moved := w.ResetsAt == nil || (w.ResetsAt.Sub(p.ResetsAt) >= 10*time.Minute && (p.Warmed.IsZero() || w.ResetsAt.Sub(p.Warmed.Add(time.Duration(w.SpanSeconds)*time.Second)) >= 10*time.Minute))
		if moved || !now.Before(p.ResetsAt) {
			return true
		}
	}
	return p.ResetsAt.IsZero() && p.Used == 0 && idleWindow(w, now) && !now.Before(p.Retry)
}
func warmRetry(idle int) time.Duration {
	wait := 15 * time.Minute
	for i := 0; i < idle && wait < 4*time.Hour; i++ {
		wait *= 2
	}
	if wait > 4*time.Hour {
		return 4 * time.Hour
	}
	return wait
}

// The original ledger used quotaDurationName(span), not the wire primary and
// secondary keys. Preserve the exact source bytes separately, and transfer its
// live record only after one current window proves an unambiguous span match.
func legacyWindowName(seconds int64) string {
	if seconds > 0 && seconds%86400 == 0 {
		return fmt.Sprintf("%d days", seconds/86400)
	}
	if seconds > 0 && seconds%3600 == 0 {
		return fmt.Sprintf("%d hours", seconds/3600)
	}
	return "Allowance"
}
func reconcileLegacy(l *AccountLedger, windows map[string]Window) bool {
	aliases := map[string][]string{}
	for name, w := range windows {
		old := legacyWindowName(w.SpanSeconds)
		aliases[old] = append(aliases[old], name)
	}
	for old, names := range aliases {
		record, exists := l.Windows[old]
		pending := false
		if l.Warm != nil {
			for _, name := range l.Warm.Windows {
				if name == old {
					pending = true
				}
			}
		}
		if !exists && !pending {
			continue
		}
		if len(names) != 1 {
			return false
		}
		name := names[0]
		if exists && old != name {
			if _, already := l.Windows[name]; already {
				return false
			}
			l.Windows[name] = record
			delete(l.Windows, old)
		}
		if l.Warm != nil {
			for i, n := range l.Warm.Windows {
				if n == old {
					l.Warm.Windows[i] = name
				}
			}
		}
	}
	return true
}
func (e Engine) warm(ctx context.Context, tx *transaction, account string, policy AccountPolicy, l *AccountLedger, s Snapshot, now time.Time) (string, error) {
	if !s.UsageKnown || !s.ModelsKnown || (s.NoCredits && s.Held) {
		l.LastResult = "quota_or_model_unavailable"
		return l.LastResult, tx.save()
	}
	byName := map[string]Window{}
	for _, w := range s.Windows {
		if w.SpanSeconds <= 0 || w.Aside || w.Model != "" {
			continue
		}
		short := w.SpanSeconds < 86400
		if (!short && policy.WeeklyWarmup) || (short && (policy.FiveHourWarmup || len(policy.WarmAts) > 0)) {
			byName[w.Name] = asNow(w, now)
		}
	}
	if !reconcileLegacy(l, byName) {
		l.LastResult = "legacy_window_ambiguous"
		return l.LastResult, tx.save()
	}
	if l.Warm != nil && (l.Warm.Phase == "sending" || l.Warm.Phase == "uncertain") {
		active := len(l.Warm.Windows) > 0
		for _, name := range l.Warm.Windows {
			w, ok := byName[name]
			if !ok || idleWindow(w, now) {
				active = false
				break
			}
		}
		if !active {
			l.LastResult = "warmup_outcome_unknown"
			return l.LastResult, tx.save()
		}
		// A fresh active window is sufficient to avoid another warmup, not proof
		// that our earlier uncertain request was the one which started it.
		for _, name := range l.Warm.Windows {
			p := observed(byName[name], l.Windows[name])
			p.Idle = 0
			p.Retry = time.Time{}
			if day := l.Warm.Daily[name]; day != "" {
				p.Daily = day
			}
			l.Windows[name] = p
		}
		l.Warm = nil
		l.LastResult = "window_observed_active"
		return l.LastResult, tx.save()
	}
	due := []string{}
	days := map[string]string{}
	onReset := map[string]bool{}
	waitDay := false
	for name, w := range byName {
		p, seen := l.Windows[name]
		short := w.SpanSeconds < 86400
		reset := (policy.FiveHourWarmup || (!short && policy.WeeklyWarmup)) && needsWarm(p, seen, w, now)
		held := short && heldForClock(policy.WarmAts, w.SpanSeconds, now)
		day, daily := "", false
		if short {
			day, daily = dailyDue(policy.WarmAts, p.Daily, w, now)
		}
		if held && !daily {
			reset = false
		}
		if short && idleWindow(w, now) && held && !daily {
			waitDay = true
		}
		if reset || daily {
			due = append(due, name)
			onReset[name] = reset
			if daily {
				days[name] = day
			}
		} else if !held {
			n := observed(w, p)
			if !idleWindow(w, now) {
				n.Idle = 0
				n.Retry = time.Time{}
			}
			l.Windows[name] = n
		}
	}
	if waitDay {
		kept := due[:0]
		for _, name := range due {
			if byName[name].SpanSeconds < 86400 {
				kept = append(kept, name)
			}
		}
		due = kept
	}
	sort.Strings(due)
	if len(due) == 0 {
		l.LastResult = "not_due"
		return "", tx.save()
	}
	if err := ctx.Err(); err != nil {
		return "deadline", nil
	}
	l.Warm = &WarmIntent{Phase: "prepared", Windows: due, PreparedAt: now, Daily: days}
	if err := tx.save(); err != nil {
		return "", err
	}
	// This durable transition happens BEFORE handing authority to the host.
	// A crash afterward is ambiguous even when no packet actually left.
	l.Warm.Phase = "sending"
	l.Warm.AttemptAt = now
	if err := tx.save(); err != nil {
		return "", err
	}
	reply, callErr := e.Host.Warmup(ctx, account, due)
	if callErr == nil && reply.Status == "completed" {
		for _, name := range due {
			p := l.Windows[name]
			n := observed(byName[name], p)
			n.Warmed = now
			n.Pending = false
			n.Failed = 0
			if day := days[name]; day != "" {
				n.Daily = day
			}
			if onReset[name] && idleWindow(byName[name], now) {
				n.Idle = p.Idle + 1
				n.Retry = now.Add(warmRetry(p.Idle))
			}
			l.Windows[name] = n
		}
		l.Warm = nil
		l.LastResult = "warmup_completed"
	} else if callErr == nil && reply.Status == "not_sent" {
		l.Warm = nil
		l.LastResult = "warmup_not_sent"
	} else {
		l.Warm.Phase = "uncertain"
		l.LastResult = "warmup_outcome_unknown"
	}
	return l.LastResult, tx.save()
}

func cardDue(s Snapshot, p AccountPolicy, now time.Time, card ResetCard) bool {
	if !s.UsageKnown || !s.CardsComplete || (card.ExpiresAt == nil || !card.ExpiresAt.After(now)) {
		return false
	}
	used := false
	blocked := false
	unknownReset := false
	var back time.Time
	for _, w := range s.Windows {
		if w.Aside {
			continue
		}
		if w.Used > 0 {
			used = true
		}
		if w.Model == "" && w.Used >= 100 && (w.ResetsAt == nil || w.ResetsAt.After(now)) {
			blocked = true
			if w.ResetsAt == nil {
				unknownReset = true
			} else if w.ResetsAt.After(back) {
				back = *w.ResetsAt
			}
		}
	}
	if !used {
		return false
	}
	at := card.ExpiresAt.Add(-time.Duration(p.ResetLeadMinutes) * time.Minute)
	return !at.After(now) || (blocked && (unknownReset || !back.Before(at)))
}
func firstCard(s Snapshot, p AccountPolicy, l *AccountLedger, now time.Time) *ResetCard {
	if !s.CardsComplete {
		return nil
	}
	cards := append([]ResetCard{}, s.Cards...)
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].ExpiresAt == nil {
			return cards[j].ExpiresAt == nil && cards[i].ID < cards[j].ID
		}
		if cards[j].ExpiresAt == nil {
			return true
		}
		if cards[i].ExpiresAt.Equal(*cards[j].ExpiresAt) {
			return cards[i].ID < cards[j].ID
		}
		return cards[i].ExpiresAt.Before(*cards[j].ExpiresAt)
	})
	for _, card := range cards {
		if _, done := l.CompletedCards[card.ID]; done {
			continue
		}
		if card.ExpiresAt == nil || !card.ExpiresAt.After(now) {
			continue
		}
		if cardDue(s, p, now, card) {
			return &card
		}
		return nil // only the earliest still-usable card is eligible
	}
	return nil
}
func nextResetCheck(s Snapshot, p AccountPolicy, now time.Time) time.Time {
	var soon time.Time
	for _, c := range s.Cards {
		if c.ExpiresAt != nil && c.ExpiresAt.After(now) && (soon.IsZero() || c.ExpiresAt.Before(soon)) {
			soon = *c.ExpiresAt
		}
	}
	if !s.UsageKnown || !s.CardsComplete {
		return now.Add(time.Minute)
	}
	if soon.IsZero() {
		return now.Add(6 * time.Hour)
	}
	lead := time.Duration(p.ResetLeadMinutes) * time.Minute
	near := lead + 30*time.Minute
	if near < time.Hour {
		near = time.Hour
	}
	every := 30 * time.Minute
	if !now.Before(soon.Add(-near)) {
		every = 5 * time.Minute
	}
	next := now.Add(every)
	for _, boundary := range []time.Time{soon.Add(-near), soon.Add(-lead)} {
		if boundary.After(now) && boundary.Before(next) {
			next = boundary
		}
	}
	return next
}
func newRedeemID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func (e Engine) reset(ctx context.Context, tx *transaction, account string, p AccountPolicy, l *AccountLedger, s Snapshot, now time.Time) (bool, string, error) {
	if l.Reset != nil && l.Reset.Purpose == "week" {
		l.LastResult = "weekly_reset_pending_review"
		return true, l.LastResult, tx.save()
	}
	candidate := firstCard(s, p, l, now)
	if l.Reset != nil {
		// Uncertain work can only revisit its original card and durable request
		// ID. A vanished card does not authorize selecting the next card.
		if candidate == nil || candidate.ID != l.Reset.CreditID || !candidate.ExpiresAt.Equal(l.Reset.ExpiresAt) {
			l.LastResult = "reset_pending_review"
			return false, l.LastResult, tx.save()
		}
	} else {
		if candidate == nil {
			return false, "", nil
		}
		if len(l.CompletedCards) >= 1024 {
			return false, "reset_history_full", nil
		}
		id, err := newRedeemID()
		if err != nil {
			return false, "", err
		}
		l.Reset = &ResetIntent{Phase: "prepared", CreditID: candidate.ID, RedeemRequestID: id, ExpiresAt: *candidate.ExpiresAt, PreparedAt: now}
		if err = tx.save(); err != nil {
			return false, "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, "deadline", nil
	}
	fresh, err := e.Host.Inspect(ctx, account)
	if err != nil || !freshSnapshot(fresh, account, e.clock()) {
		l.LastResult = "reset_recheck_unavailable"
		return false, l.LastResult, tx.save()
	}
	if e.Observe != nil {
		_ = e.Observe(fresh)
	}
	exact := firstCard(fresh, p, l, e.clock())
	if exact == nil || exact.ID != l.Reset.CreditID || !exact.ExpiresAt.Equal(l.Reset.ExpiresAt) {
		l.LastResult = "reset_card_changed"
		return false, l.LastResult, tx.save()
	}
	previouslyUncertain := l.Reset.Phase == "sending" || l.Reset.Phase == "uncertain"
	l.Reset.Phase = "sending"
	l.Reset.AttemptAt = e.clock()
	if err = tx.save(); err != nil {
		return false, "", err
	}
	reply, callErr := e.Host.Consume(ctx, account, ConsumeRequest{CreditID: l.Reset.CreditID, RedeemRequestID: l.Reset.RedeemRequestID, ExpectedExpiresAt: l.Reset.ExpiresAt, LeadMinutes: p.ResetLeadMinutes, Purpose: "expiring"})
	if callErr == nil && reply.Status == "completed" && reply.Code == "nothing_to_reset" {
		// The server positively reported no spend. Preserve the card for a
		// later independently due action, without claiming it was redeemed.
		l.Reset = nil
		l.LastResult = "reset_not_needed"
		l.NextResetCheck = e.clock().Add(5 * time.Minute)
	} else if callErr == nil && reply.Status == "completed" && (reply.Code == "reset" || reply.Code == "already_redeemed" || reply.Code == "no_credit") {
		l.CompletedCards[l.Reset.CreditID] = l.Reset.ExpiresAt
		l.Reset = nil
		l.LastResult = "reset_completed"
		l.NextWarmCheck = e.clock()
		l.NextResetCheck = e.clock().Add(5 * time.Minute)
	} else if callErr == nil && reply.Status == "not_sent" {
		// Keep the prepared card/ID: later attempts remain idempotent even if a
		// prior independent invocation had already used this same request ID.
		l.Reset.Phase = "prepared"
		if previouslyUncertain {
			l.Reset.Phase = "uncertain"
		}
		l.LastResult = "reset_not_sent"
	} else {
		l.Reset.Phase = "uncertain"
		l.LastResult = "reset_outcome_unknown"
		l.NextResetCheck = e.clock().Add(time.Minute)
	}
	return true, l.LastResult, tx.save()
}
