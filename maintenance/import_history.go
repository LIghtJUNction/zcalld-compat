package maintenance

import (
	"encoding/json"
	"strings"
	"time"
)

// A missing original file is the source's legitimate "no spend yet" state.
// A present but invalid one is never silently treated as empty. Its secondary
// audit log can prove a conservative seven-day block; when neither can be read
// coherently, ApplyPlan blocks weekly consumption for a full week.
func weeklyImportHistory(primary, audit []byte) (map[string]WeeklyRecord, bool) {
	out := map[string]WeeklyRecord{}
	if primary == nil && audit == nil {
		return out, false
	}
	var records map[string]struct {
		Until   time.Time `json:"until"`
		At      time.Time `json:"at"`
		Outcome struct {
			Code    string `json:"code"`
			Windows int    `json:"windows"`
		} `json:"outcome"`
	}
	if json.Unmarshal(primary, &records) == nil && len(records) <= 4096 {
		for user, item := range records {
			user = strings.ToLower(strings.TrimSpace(user))
			if user == "" {
				continue
			}
			if item.Until.After(out[user].Until) {
				out[user] = WeeklyRecord{Until: item.Until, At: item.At, Code: "imported_spend"}
			}
		}
		return out, false
	}
	if audit == nil {
		return out, true
	}
	var uses []struct {
		User    string    `json:"user"`
		At      time.Time `json:"at"`
		Why     string    `json:"why"`
		Windows int       `json:"windows"`
	}
	if json.Unmarshal(audit, &uses) != nil || len(uses) > 4096 {
		return out, true
	}
	for _, use := range uses {
		if use.Why != "week" {
			continue
		}
		user := strings.ToLower(strings.TrimSpace(use.User))
		if user == "" || use.At.IsZero() {
			return nil, true
		}
		until := use.At.Add(7 * 24 * time.Hour)
		if until.After(out[user].Until) {
			out[user] = WeeklyRecord{Until: until, At: use.At, Code: "imported_audit"}
		}
	}
	return out, false
}
