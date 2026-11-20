package maintenance

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const dailySlack = time.Hour

func normalizedClock(value string) (string, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"15:04", "15:04:05"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.Format("15:04"), true
		}
	}
	return "", false
}

func normalizedClocks(values []string) ([]string, error) {
	if len(values) > 1440 {
		return nil, ErrInvalid
	}
	unique := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		clock, ok := normalizedClock(value)
		if !ok {
			return nil, ErrInvalid
		}
		unique[clock] = true
	}
	out := make([]string, 0, len(unique))
	for clock := range unique {
		out = append(out, clock)
	}
	sort.Strings(out)
	return out, nil
}

// Local calendar construction preserves the source's wall-clock meaning. It
// intentionally does not convert a requested 09:00 into 09:00 UTC.
func localStart(clock string, now time.Time, dayOffset int) time.Time {
	var hour, minute int
	if _, err := fmt.Sscanf(clock, "%d:%d", &hour, &minute); err != nil {
		return time.Time{}
	}
	y, m, d := now.Date()
	return time.Date(y, m, d+dayOffset, hour, minute, 0, 0, now.Location())
}

func latestStart(clocks []string, now time.Time) (time.Time, bool) {
	var latest time.Time
	for _, clock := range clocks {
		at := localStart(clock, now, 0)
		if at.After(now) {
			at = localStart(clock, now, -1)
		}
		if latest.IsZero() || at.After(latest) {
			latest = at
		}
	}
	return latest, !latest.IsZero()
}

func nextStart(clocks []string, now time.Time) (time.Time, bool) {
	var next time.Time
	for _, clock := range clocks {
		at := localStart(clock, now, 0)
		if !at.After(now) {
			at = localStart(clock, now, 1)
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next, !next.IsZero()
}

func dailyDue(clocks []string, previous string, w Window, now time.Time) (string, bool) {
	at, ok := latestStart(clocks, now)
	if !ok || now.Sub(at) > dailySlack {
		return "", false
	}
	key := at.Format("2006-01-02 15:04")
	already := previous == key || (len(previous) == len(time.DateOnly) && strings.HasPrefix(key, previous+" "))
	return key, !already && idleWindow(w, now)
}

func heldForClock(clocks []string, span int64, now time.Time) bool {
	next, ok := nextStart(clocks, now)
	return ok && next.Sub(now) < time.Duration(span)*time.Second
}

func warmPolicyEnabled(p AccountPolicy) bool {
	return p.WeeklyWarmup || p.FiveHourWarmup || len(p.WarmAts) > 0
}

func warmCheckDue(p AccountPolicy, l *AccountLedger, now time.Time) bool {
	if !warmPolicyEnabled(p) {
		return false
	}
	if !now.Before(l.NextWarmCheck) {
		return true
	}
	start, ok := latestStart(p.WarmAts, now)
	return ok && now.Sub(start) <= dailySlack && start.After(l.LastWarmCheck)
}
