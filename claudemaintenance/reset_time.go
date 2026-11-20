// Derived from yetone/magpie@1e174f8f9f613a920203a4658d259456fcab5d0d
// internal/provider/claude_usage_cli.go; MIT, copyright 2026 yetone.
package claudemaintenance

import (
	"strings"
	"time"
)

// claudeResetTime reads "Oct 1 at 3:30pm (Asia/Shanghai)", "Oct 9, 2:59pm
// (UTC)" (Claude Code 2.1.285 on) or "3pm (Asia/Shanghai)", in the zone
// named, else local time. It ends a window span long, so it is never further
// ahead than that (and claudeResetSlack): a date with no year is in the first
// year it is less than a day past and no further ahead, and a time alone is
// the next one from now no further ahead. Where none is, as for a reading
// days old or a clock that is off, it is in the latest year (or day) where
// it is already past, and the window reads as renewed (elapsed). Where the
// clocks go back and read that time twice, the reading not yet past is
// taken.
func claudeResetTime(s string, now time.Time, span time.Duration) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// a usage_report row's resets_at, which the gateway adds as written
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	loc := time.Local
	if i := strings.LastIndex(s, "("); i >= 0 && strings.HasSuffix(s, ")") {
		if l, err := time.LoadLocation(s[i+1 : len(s)-1]); err == nil {
			loc = l
		}
		s = strings.TrimSpace(s[:i])
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "AM", "am"), "PM", "pm")
	ref := now.In(loc)
	latest := ref.Add(span + claudeResetSlack)
	for _, layout := range []string{
		"Jan 2 at 3:04pm", "Jan 2 at 3pm", "Jan 2, 2006 at 3:04pm", "Jan 2, 2006 at 3pm",
		"Jan 2, 3:04pm", "Jan 2, 3pm", "Jan 2, 2006, 3:04pm", "Jan 2, 2006, 3pm",
	} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		if t.Year() != 0 {
			return t, true
		}
		// Dec 31 read just after New Year is last year's
		var past time.Time
		for y := ref.Year() - 1; y <= ref.Year()+1; y++ {
			d := time.Date(y, t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if r, ok := nextReading(d, ref, ref.Add(-24*time.Hour)); ok && !r.After(latest) {
				return r, true
			}
			if d.Before(ref) {
				past = d
			}
		}
		return past, true
	}
	for _, layout := range []string{"3:04pm", "3pm"} {
		c, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		// a day whose clocks skip that time is passed over: Santiago's skip
		// midnight, so "12:30am" before it is the day after tomorrow's
		for d := 0; d <= 2; d++ {
			t := time.Date(ref.Year(), ref.Month(), ref.Day()+d, c.Hour(), c.Minute(), 0, 0, loc)
			if t.Hour() != c.Hour() || t.Minute() != c.Minute() {
				continue
			}
			if r, ok := nextReading(t, ref, ref); ok {
				if !r.After(latest) {
					return r, true
				}
				break
			}
		}
		for d := 0; d >= -2; d-- {
			t := time.Date(ref.Year(), ref.Month(), ref.Day()+d, c.Hour(), c.Minute(), 0, 0, loc)
			if t.Hour() == c.Hour() && t.Minute() == c.Minute() && t.Before(ref) {
				return t, true
			}
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// claudeResetSlack is how much further ahead than its window a reset may
// read, as this machine's clock may be behind Anthropic's. Claude Code
// prints the reset from the instant Anthropic gives, seconds dropped, so it
// is never later than that.
const claudeResetSlack = 2 * time.Hour

// nextReading is the first instant whose clock reads as t's does that isn't
// before ref, else the first that isn't before lim. As the clocks go back, an
// hour is read twice, and time.Date may give either instant.
func nextReading(t, ref, lim time.Time) (time.Time, bool) {
	first, last := t, t
	start, end := t.ZoneBounds()
	_, off := t.Zone()
	if !start.IsZero() {
		_, prev := start.Add(-time.Second).Zone()
		if e := t.Add(time.Duration(off-prev) * time.Second); prev > off && e.Before(start) {
			first = e
		}
	}
	if !end.IsZero() {
		_, next := end.Zone()
		if l := t.Add(time.Duration(off-next) * time.Second); next < off && !l.Before(end) {
			last = l
		}
	}
	for _, from := range []time.Time{ref, lim} {
		for _, r := range []time.Time{first, last} {
			if !r.Before(from) {
				return r, true
			}
		}
	}
	return time.Time{}, false
}
