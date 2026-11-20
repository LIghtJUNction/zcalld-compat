package maintenance

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// WarmAccounts is an explicit, bounded action independent of the scheduler's
// enabled flag. Existing uncertain intents still prevent blind repetition.
// all skips accounts whose general windows are already running. Each task has
// its own host; at most three sends run concurrently, under one durable lock.
func (e Engine) WarmAccounts(ctx context.Context, accounts []string, all bool) (Report, error) {
	result := Report{Status: "complete", Results: []AccountResult{}}
	if len(accounts) == 0 || len(accounts) > MaxAccounts {
		return result, ErrInvalid
	}
	keys := append([]string(nil), accounts...)
	sort.Strings(keys)
	for i, key := range keys {
		if !accountPattern.MatchString(key) || (i > 0 && key == keys[i-1]) {
			return result, ErrInvalid
		}
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultBudget)
	defer cancel()
	tx, err := e.Store.acquire(ctx, true)
	if err != nil {
		return result, err
	}
	defer tx.close()
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan string)
	var wg sync.WaitGroup
	work := func(account string) {
		host, closeHost := e.Host, func() {}
		if e.ManualHost != nil {
			host, closeHost = e.ManualHost(account)
		}
		if closeHost == nil {
			closeHost = func() {}
		}
		defer closeHost() // drain precedes release of the process/ledger lock
		status := "not_sent"
		finish := func() {
			mu.Lock()
			defer mu.Unlock()
			result.Results = append(result.Results, AccountResult{account, "warmup", status})
		}
		defer finish()
		if host == nil || ctx.Err() != nil {
			return
		}
		mu.Lock()
		l := tx.doc.Ledger[account]
		if l == nil {
			l = &AccountLedger{Windows: map[string]WarmWindow{}, CompletedCards: map[string]time.Time{}}
			tx.doc.Ledger[account] = l
		}
		if l.Windows == nil {
			l.Windows = map[string]WarmWindow{}
		}
		pending := l.Warm != nil
		mu.Unlock()
		if pending {
			status = "pending_review"
			return
		}
		snapshot, readErr := host.Inspect(ctx, account)
		now := e.clock()
		if readErr != nil || !freshSnapshot(snapshot, account, now) || !snapshot.UsageKnown || !snapshot.ModelsKnown || (snapshot.NoCredits && snapshot.Held) {
			status = "quota_or_model_unavailable"
			return
		}
		windows := []string{}
		running := true
		for _, window := range snapshot.Windows {
			if window.SpanSeconds <= 0 || window.Aside || window.Model != "" {
				continue
			}
			windows = append(windows, window.Name)
			running = running && !idleWindow(asNow(window, now), now)
		}
		if len(windows) == 0 {
			status = "no_plan_windows"
			return
		}
		if all && running {
			status = "running"
			return
		}
		sort.Strings(windows)
		mu.Lock()
		l.Warm = &WarmIntent{Phase: "prepared", Windows: windows, PreparedAt: now}
		saveErr := tx.save()
		if saveErr == nil && ctx.Err() == nil {
			l.Warm.Phase = "sending"
			l.Warm.AttemptAt = e.clock()
			saveErr = tx.save()
		}
		if saveErr != nil {
			if firstErr == nil {
				firstErr = saveErr
			}
			cancel()
		}
		mu.Unlock()
		if saveErr != nil || ctx.Err() != nil {
			return
		}
		reply, sendErr := host.Warmup(ctx, account, windows)
		mu.Lock()
		switch {
		case sendErr == nil && reply.Status == "completed":
			status = "completed"
			for _, window := range snapshot.Windows {
				if window.SpanSeconds <= 0 || window.Aside || window.Model != "" {
					continue
				}
				previous := l.Windows[window.Name]
				n := observed(window, previous)
				n.Warmed = e.clock()
				n.Pending = false
				n.Failed = 0
				l.Windows[window.Name] = n
			}
			l.Warm = nil
		case sendErr == nil && reply.Status == "not_sent":
			status = "not_sent"
			l.Warm = nil
		default:
			status = "unknown"
			l.Warm.Phase = "uncertain"
		}
		l.LastResult = status
		if err := tx.save(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			cancel()
		}
		mu.Unlock()
	}
	for range min(3, len(keys)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for account := range jobs {
				work(account)
			}
		}()
	}
	for _, key := range keys {
		jobs <- key
	}
	close(jobs)
	wg.Wait()
	sort.Slice(result.Results, func(i, j int) bool { return result.Results[i].Account < result.Results[j].Account })
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status = "deadline"
	}
	return result, firstErr
}
