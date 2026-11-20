package claudemaintenance

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

type usage struct {
	Windows      []maintenance.Window
	Known        bool
	ExtraKnown   bool
	ExtraEnabled bool
	ExtraUsed    float64
	ExtraLimit   float64
}
type usageRow struct {
	Kind    string   `json:"kind"`
	Percent *float64 `json:"percent"`
	Resets  *string  `json:"resets_at"`
	Scope   *struct {
		Model *struct {
			Name string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}
type limits struct {
	Rows  json.RawMessage `json:"limits"`
	Five  json.RawMessage `json:"five_hour"`
	Week  json.RawMessage `json:"seven_day"`
	Extra *struct {
		Enabled bool     `json:"is_enabled"`
		Used    *float64 `json:"used_credits"`
		Limit   *float64 `json:"monthly_limit"`
	} `json:"extra_usage"`
}

var usageLine = regexp.MustCompile(`^Current (session|week(?: \(([^)]+)\))?):\s*([0-9.]+)% used(?:\s*·\s*resets (.+))?$`)
var ansi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func validPercent(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 100 }
func parseUsage(b []byte, now time.Time) (usage, error) {
	out := usage{Windows: []maintenance.Window{}}
	var messages []json.RawMessage
	if json.Unmarshal(b, &messages) != nil {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return out, errors.New("Claude usage response is invalid")
		}
		messages = []json.RawMessage{b}
	}
	if len(messages) > 128 {
		return out, ErrConfiguration
	}
	text := ""
	haveResult := false
	haveLimits := false
	for _, raw := range messages {
		var m struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Report  *struct {
				Limits *limits `json:"rate_limits"`
			} `json:"usage_report"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return out, ErrConfiguration
		}
		if m.Type == "result" {
			if m.IsError {
				return out, errors.New("Claude usage read failed")
			}
			text = m.Result
			haveResult = true
		}
		if m.Report == nil || m.Report.Limits == nil {
			continue
		}
		if haveLimits {
			return out, errors.New("Claude usage report is ambiguous")
		}
		haveLimits = true
		l := m.Report.Limits
		if l.Extra != nil {
			out.ExtraEnabled = l.Extra.Enabled
			if !l.Extra.Enabled {
				out.ExtraKnown = true
			} else if l.Extra.Limit != nil && l.Extra.Used != nil && *l.Extra.Limit > 0 && *l.Extra.Used >= 0 && !math.IsInf(*l.Extra.Limit, 0) && !math.IsInf(*l.Extra.Used, 0) {
				out.ExtraKnown = true
				out.ExtraLimit = *l.Extra.Limit / 100
				out.ExtraUsed = *l.Extra.Used / 100
			}
		}
		if len(l.Rows) > 0 && string(l.Rows) != "null" {
			var rows []usageRow
			if json.Unmarshal(l.Rows, &rows) != nil || len(rows) > 64 {
				return out, ErrConfiguration
			}
			out.Known = true // [] is an explicit plan without meters; never invent windows.
			for _, row := range rows {
				if row.Percent == nil || !validPercent(*row.Percent) {
					return out, ErrConfiguration
				}
				w := maintenance.Window{Used: *row.Percent}
				switch row.Kind {
				case "session":
					w.Name = "5 hours"
					w.SpanSeconds = 5 * 3600
				case "weekly_all":
					w.Name = "7 days"
					w.SpanSeconds = 7 * 86400
				case "weekly_scoped":
					if row.Scope == nil || row.Scope.Model == nil || strings.TrimSpace(row.Scope.Model.Name) == "" {
						continue
					}
					w.Name = "7 days · " + row.Scope.Model.Name
					w.Model = row.Scope.Model.Name
					w.SpanSeconds = 7 * 86400
				default:
					continue
				}
				if row.Resets != nil && *row.Resets != "" {
					t, err := time.Parse(time.RFC3339Nano, *row.Resets)
					if err != nil {
						return out, ErrConfiguration
					}
					w.ResetsAt = &t
				}
				out.Windows = append(out.Windows, w)
			}
		} else if l.Five != nil || l.Week != nil {
			for i, raw := range []json.RawMessage{l.Five, l.Week} {
				// null is not a successful unused-window reading. Claude's
				// text may prove one; otherwise this meter remains unknown.
				if raw == nil || string(raw) == "null" {
					continue
				}
				w := maintenance.Window{Name: "5 hours", SpanSeconds: 5 * 3600}
				if i == 1 {
					w.Name = "7 days"
					w.SpanSeconds = 7 * 86400
				}
				if string(raw) != "null" {
					var v struct {
						Used   *float64 `json:"utilization"`
						Resets *string  `json:"resets_at"`
					}
					if json.Unmarshal(raw, &v) != nil || v.Used == nil || !validPercent(*v.Used) {
						return out, ErrConfiguration
					}
					w.Used = *v.Used
					if v.Resets != nil && *v.Resets != "" {
						t, err := time.Parse(time.RFC3339Nano, *v.Resets)
						if err != nil {
							return out, ErrConfiguration
						}
						w.ResetsAt = &t
					}
				}
				out.Windows = append(out.Windows, w)
			}
			out.Known = len(out.Windows) > 0
		}
	}
	if !haveResult {
		return out, errors.New("Claude usage did not complete")
	}
	if !out.Known {
		text = strings.TrimSpace(ansi.ReplaceAllString(text, ""))
		for _, line := range strings.Split(text, "\n") {
			m := usageLine.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			n, err := strconv.ParseFloat(m[3], 64)
			if err != nil || !validPercent(n) {
				return out, ErrConfiguration
			}
			w := maintenance.Window{Name: "5 hours", Used: n, SpanSeconds: 5 * 3600}
			if m[1] != "session" {
				w.Name = "7 days"
				w.SpanSeconds = 7 * 86400
				if m[2] != "" && !strings.EqualFold(m[2], "all models") {
					w.Name += " · " + m[2]
					w.Model = m[2]
				}
			}
			if m[4] != "" {
				t, ok := claudeResetTime(m[4], now, time.Duration(w.SpanSeconds)*time.Second)
				if !ok {
					return out, ErrConfiguration
				}
				w.ResetsAt = &t
			}
			out.Windows = append(out.Windows, w)
		}
		out.Known = len(out.Windows) > 0
	}
	seen := map[string]bool{}
	for _, w := range out.Windows {
		if seen[w.Name] {
			return out, ErrConfiguration
		}
		seen[w.Name] = true
	}
	if !out.Known {
		return out, errors.New("Claude plan usage is unavailable")
	}
	return out, nil
}

func (u usage) held(a account, now time.Time) (bool, error) {
	restrict := a.NoExtra
	if a.ExtraShare > 0 || a.ExtraAmount > 0 {
		if !u.ExtraKnown {
			return false, errors.New("Claude extra usage is unavailable for its cap")
		}
		if u.ExtraEnabled && u.ExtraLimit > 0 {
			restrict = restrict || (a.ExtraAmount > 0 && u.ExtraUsed >= a.ExtraAmount) || (a.ExtraShare > 0 && u.ExtraUsed/u.ExtraLimit*100 >= float64(a.ExtraShare))
		}
	}
	if !restrict {
		return false, nil
	}
	for _, w := range u.Windows {
		if w.Aside || w.Model != "" {
			continue
		}
		if w.ResetsAt != nil && !w.ResetsAt.After(now) {
			continue
		}
		if w.Used >= 100 {
			return true, nil
		}
	}
	return false, nil
}
