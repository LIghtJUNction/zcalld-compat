package maintenance

import (
	"context"
	"errors"
	"sort"
	"time"
)

func validTrigger(trigger WeeklyTrigger) bool {
	return trigger == WeeklyBeforeCredits || trigger == WeeklyRoutesExhausted
}

func weeklyUntil(s Snapshot, trigger WeeklyTrigger, now time.Time) time.Time {
	if !s.UsageKnown {
		return time.Time{}
	}
	if trigger == WeeklyBeforeCredits && (s.NoCredits || !s.OnCredits) {
		return time.Time{}
	}
	var until time.Time
	for _, w := range s.Windows {
		if weekly(w) && w.Used >= 100 && w.ResetsAt != nil && w.ResetsAt.After(now) && w.ResetsAt.After(until) {
			until = *w.ResetsAt
		}
	}
	return until
}

func cardExpiry(c ResetCard) time.Time {
	if c.ExpiresAt == nil {
		return time.Time{}
	}
	return *c.ExpiresAt
}

func weeklyCard(s Snapshot, l *AccountLedger, now time.Time) *ResetCard {
	if !s.CardsComplete {
		return nil
	}
	cards := make([]ResetCard, 0, len(s.Cards))
	for _, c := range s.Cards {
		if c.ExpiresAt != nil && !c.ExpiresAt.After(now) {
			continue
		}
		if _, used := l.CompletedCards[c.ID]; used {
			continue
		}
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool {
		a, b := cards[i].ExpiresAt, cards[j].ExpiresAt
		if a == nil && b != nil {
			return false
		}
		if a != nil && b == nil {
			return true
		}
		if a != nil && !a.Equal(*b) {
			return a.Before(*b)
		}
		return cards[i].ID < cards[j].ID
	})
	if len(cards) == 0 {
		return nil
	}
	return &cards[0]
}

// RunWeekly is a request-path operation, never a scheduler operation. The
// trusted native caller supplies an explicit account and the routing fact that
// all alternatives were exhausted, or that this request is about to use the
// account's credits. Neither fact can be supplied by an HTTP client header.
// Before-credits additionally requires fresh server evidence and permission.
// A successful result with code "reset" is the only authorization to retry.
func (e Engine) RunWeekly(ctx context.Context, account string, trigger WeeklyTrigger) (ActionResult, error) {
	out := ActionResult{Status: "not_sent", Code: "weekly_disabled"}
	if !accountPattern.MatchString(account) || !validTrigger(trigger) {
		return out, ErrInvalid
	}
	d, err := e.Store.Read()
	if err != nil {
		return out, err
	}
	if !d.Policy.Enabled || !d.Policy.Accounts[account].WeeklyReset {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultBudget)
	defer cancel()
	tx, err := e.Store.acquire(ctx, false)
	if errors.Is(err, ErrBusy) {
		return ActionResult{Status: "not_sent", Code: "maintenance_busy"}, nil
	}
	if err != nil {
		return out, err
	}
	defer tx.close()
	if !tx.doc.Policy.Enabled || !tx.doc.Policy.Accounts[account].WeeklyReset {
		return out, nil
	}
	l := tx.doc.Ledger[account]
	if l == nil {
		l = &AccountLedger{Windows: map[string]WarmWindow{}, CompletedCards: map[string]time.Time{}}
		tx.doc.Ledger[account] = l
	}
	if l.CompletedCards == nil {
		l.CompletedCards = map[string]time.Time{}
	}
	now := e.clock()
	if now.Before(l.Weekly.Until) {
		return ActionResult{Status: "not_sent", Code: "weekly_already_used"}, nil
	}
	if now.Before(l.NextWeeklyCheck) {
		return ActionResult{Status: "not_sent", Code: "weekly_backoff"}, nil
	}
	if l.Reset != nil && l.Reset.Purpose != "week" {
		return ActionResult{Status: "not_sent", Code: "reset_pending_review"}, nil
	}
	if e.Host == nil {
		return out, errors.New("maintenance host is unavailable")
	}
	snapshot, err := e.Host.Inspect(ctx, account)
	now = e.clock()
	if err != nil || !freshSnapshot(snapshot, account, now) {
		l.NextWeeklyCheck = now.Add(10 * time.Minute)
		l.LastResult = "weekly_inspection_unavailable"
		return ActionResult{Status: "not_sent", Code: l.LastResult}, tx.save()
	}
	if e.Observe != nil {
		_ = e.Observe(snapshot)
	}
	until := weeklyUntil(snapshot, trigger, now)
	card := weeklyCard(snapshot, l, now)
	if until.IsZero() || card == nil {
		l.NextWeeklyCheck = now.Add(time.Minute)
		if !until.IsZero() {
			l.NextWeeklyCheck = now.Add(10 * time.Minute)
		}
		l.LastResult = "weekly_not_due"
		return ActionResult{Status: "not_sent", Code: l.LastResult}, tx.save()
	}
	if l.Reset != nil {
		if card.ID != l.Reset.CreditID || !cardExpiry(*card).Equal(l.Reset.ExpiresAt) || !until.Equal(l.Reset.WeekUntil) {
			l.LastResult = "reset_pending_review"
			return ActionResult{Status: "not_sent", Code: l.LastResult}, tx.save()
		}
	} else {
		if len(l.CompletedCards) >= 1024 {
			return ActionResult{Status: "not_sent", Code: "reset_history_full"}, nil
		}
		id, e := newRedeemID()
		if e != nil {
			return out, e
		}
		l.Reset = &ResetIntent{Phase: "prepared", CreditID: card.ID, RedeemRequestID: id, ExpiresAt: cardExpiry(*card), PreparedAt: now, Purpose: "week", WeekUntil: until}
		if err = tx.save(); err != nil {
			return out, err
		}
	}
	if ctx.Err() != nil {
		return ActionResult{Status: "not_sent", Code: "deadline"}, nil
	}
	fresh, err := e.Host.Inspect(ctx, account)
	now = e.clock()
	if err != nil || !freshSnapshot(fresh, account, now) {
		l.LastResult = "weekly_recheck_unavailable"
		return ActionResult{Status: "not_sent", Code: l.LastResult}, tx.save()
	}
	if e.Observe != nil {
		_ = e.Observe(fresh)
	}
	exact := weeklyCard(fresh, l, now)
	if exact == nil || exact.ID != l.Reset.CreditID || !cardExpiry(*exact).Equal(l.Reset.ExpiresAt) || !weeklyUntil(fresh, trigger, now).Equal(l.Reset.WeekUntil) {
		l.LastResult = "weekly_recheck_changed"
		return ActionResult{Status: "not_sent", Code: l.LastResult}, tx.save()
	}
	wasUnknown := l.Reset.Phase == "sending" || l.Reset.Phase == "uncertain"
	l.Reset.Phase = "sending"
	l.Reset.AttemptAt = now
	if err = tx.save(); err != nil {
		return out, err
	}
	out = ActionResult{Status: "unknown", Code: "reset_outcome_unknown"}
	reply, callErr := e.Host.Consume(ctx, account, ConsumeRequest{CreditID: l.Reset.CreditID, RedeemRequestID: l.Reset.RedeemRequestID, ExpectedExpiresAt: l.Reset.ExpiresAt, LeadMinutes: tx.doc.Policy.Accounts[account].ResetLeadMinutes, Purpose: "week", Trigger: trigger, ExpectedWeekUntil: l.Reset.WeekUntil})
	now = e.clock()
	switch {
	case callErr == nil && reply.Status == "completed" && (reply.Code == "reset" || reply.Code == "already_redeemed"):
		l.Weekly = WeeklyRecord{Until: l.Reset.WeekUntil, At: now, Code: reply.Code}
		l.CompletedCards[l.Reset.CreditID] = l.Reset.ExpiresAt
		l.Reset = nil
		l.LastResult = "weekly_reset_completed"
		l.NextWarmCheck = now
		l.NextResetCheck = now.Add(5 * time.Minute)
		out = reply
	case callErr == nil && reply.Status == "completed" && (reply.Code == "nothing_to_reset" || reply.Code == "no_credit"):
		if reply.Code == "no_credit" {
			l.CompletedCards[l.Reset.CreditID] = l.Reset.ExpiresAt
		}
		l.Reset = nil
		l.NextWeeklyCheck = now.Add(10 * time.Minute)
		l.LastResult = "weekly_reset_not_needed"
		out = reply
	case callErr == nil && reply.Status == "not_sent":
		l.Reset.Phase = "prepared"
		if wasUnknown {
			l.Reset.Phase = "uncertain"
		}
		l.NextWeeklyCheck = now.Add(time.Minute)
		l.LastResult = "weekly_reset_not_sent"
		out = ActionResult{Status: "not_sent", Code: "weekly_reset_not_sent"}
	default:
		l.Reset.Phase = "uncertain"
		l.NextWeeklyCheck = now.Add(time.Minute)
		l.LastResult = "weekly_reset_outcome_unknown"
	}
	if err = tx.save(); err != nil {
		// The remote may already have consumed the card. The persisted sending
		// intent remains the source of truth; never tell routing to retry yet.
		return ActionResult{Status: "unknown", Code: "reset_ledger_unavailable"}, err
	}
	return out, nil
}
