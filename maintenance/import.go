package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// AccountLink is supplied by the existing import mapper after matching an
// original snapshot login with its already-imported opaque account. It is not
// built by guessing the hash suffix or by importing credentials again.
type AccountLink struct {
	SourceUser string
	Account    string
	Enabled    bool
}

type ImportPlan struct {
	Adapter              json.RawMessage                  `json:"adapter,omitempty"`
	Accounts             map[string]AccountPolicy         `json:"accounts"`
	Windows              map[string]map[string]WarmWindow `json:"windows"`
	LegacyWarmup         []byte                           `json:"legacy_warmup"`
	LegacyAutoReset      []byte                           `json:"legacy_auto_reset,omitempty"`
	LegacyResetsUsed     []byte                           `json:"legacy_resets_used,omitempty"`
	WeeklyRecords        map[string]WeeklyRecord          `json:"weekly_records,omitempty"`
	WeeklyHistoryUnknown bool                             `json:"weekly_history_unknown,omitempty"`
	SourceDigest         string                           `json:"source_digest"`
	UnmappedAccounts     int                              `json:"unmapped_accounts"`
	Hash                 string                           `json:"hash"`
}

type legacySettings struct {
	Warmup        string            `json:"codexWarmup"`
	WarmAt        string            `json:"codexWarmAt"`
	WarmAts       []string          `json:"codexWarmAts"`
	WarmAtOf      map[string]string `json:"codexWarmAtOf"`
	AutoReset     []string          `json:"codexAutoReset"`
	AutoResetOnly map[string]string `json:"codexAutoResetOnly"`
	ResetLead     int               `json:"codexResetLead"`
}

// SealPlan binds adapter-specific immutable planning inputs using the same plan hash.
func SealPlan(p *ImportPlan) { p.Hash = planHash(*p) }

func planHash(p ImportPlan) string {
	p.Hash = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PlanImport is local-only and imports no credentials or accounts. The extended
// form additionally preserves the source's once-per-week redemption history.
func PlanImport(settings, warmup []byte, links []AccountLink) (ImportPlan, error) {
	return PlanImportLedgers(settings, warmup, nil, nil, links)
}
func PlanImportLedgers(settings, warmup, autoReset, resetsUsed []byte, links []AccountLink) (ImportPlan, error) {
	p := ImportPlan{Accounts: map[string]AccountPolicy{}, Windows: map[string]map[string]WarmWindow{}, WeeklyRecords: map[string]WeeklyRecord{}, LegacyWarmup: append([]byte{}, warmup...), LegacyAutoReset: append([]byte{}, autoReset...), LegacyResetsUsed: append([]byte{}, resetsUsed...)}
	if len(settings) > 2<<20 || len(warmup)+len(autoReset)+len(resetsUsed) > 2<<20 || len(links) > MaxAccounts {
		return p, ErrInvalid
	}
	var s legacySettings
	if json.Unmarshal(settings, &s) != nil {
		return p, ErrInvalid
	}
	if s.Warmup != "" && s.Warmup != "off" && s.Warmup != "week" && s.Warmup != "all" {
		return p, ErrInvalid
	}
	if s.WarmAts == nil && strings.TrimSpace(s.WarmAt) != "" {
		s.WarmAts = []string{s.WarmAt}
	}
	clocks, err := normalizedClocks(s.WarmAts)
	if err != nil {
		return p, err
	}
	ownClocks := map[string][]string{}
	for user, value := range s.WarmAtOf {
		user = strings.ToLower(strings.TrimSpace(user))
		value = strings.ToLower(strings.TrimSpace(value))
		if user == "" || value == "" {
			continue
		}
		at := []string{}
		if value != "off" {
			at, err = normalizedClocks([]string{value})
			if err != nil {
				return p, err
			}
		}
		if previous, ok := ownClocks[user]; ok && strings.Join(previous, ",") != strings.Join(at, ",") {
			return p, ErrInvalid
		}
		ownClocks[user] = at
	}
	lead := s.ResetLead
	if lead < 3 || lead > 1440 {
		lead = 30
	}
	legacy := map[string]map[string]WarmWindow{}
	if len(warmup) > 0 && json.Unmarshal(warmup, &legacy) != nil {
		return p, ErrInvalid
	}
	if len(legacy) > 4096 {
		return p, ErrInvalid
	}
	enabled := map[string]string{}
	for _, link := range links {
		if !link.Enabled {
			continue
		}
		user := strings.ToLower(strings.TrimSpace(link.SourceUser))
		if user == "" || !accountPattern.MatchString(link.Account) {
			return p, ErrInvalid
		}
		if previous, ok := enabled[user]; ok && previous != link.Account {
			return p, errors.New("snapshot account mapping is ambiguous")
		}
		for source, key := range enabled {
			if key == link.Account && source != user {
				return p, errors.New("snapshot account mapping is ambiguous")
			}
		}
		enabled[user] = link.Account
	}
	resetUsers := map[string]bool{}
	for _, user := range s.AutoReset {
		resetUsers[strings.ToLower(strings.TrimSpace(user))] = true
	}
	resetOnly := map[string]string{}
	for user, value := range s.AutoResetOnly {
		user = strings.ToLower(strings.TrimSpace(user))
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && value != "week" && value != "expiring" {
			return p, ErrInvalid
		}
		if previous, ok := resetOnly[user]; ok && previous != value {
			return p, ErrInvalid
		}
		resetOnly[user] = value
	}
	history, unknown := weeklyImportHistory(autoReset, resetsUsed)
	p.WeeklyHistoryUnknown = unknown
	for user, account := range enabled {
		reset := resetUsers[user]
		ats := clocks
		if own, ok := ownClocks[user]; ok {
			ats = own
		}
		policy := AccountPolicy{WeeklyWarmup: s.Warmup == "week" || s.Warmup == "all", FiveHourWarmup: s.Warmup == "all", WarmAts: append([]string{}, ats...), WeeklyReset: reset && resetOnly[user] != "expiring", ExpiringReset: reset && resetOnly[user] != "week", ResetLeadMinutes: lead}
		if !warmPolicyEnabled(policy) && !policy.ExpiringReset && !policy.WeeklyReset {
			continue
		}
		p.Accounts[account] = policy
		ws := legacy[user]
		if ws == nil {
			ws = map[string]WarmWindow{}
		}
		p.Windows[account] = ws
		if policy.WeeklyReset {
			record := history[user]
			if global := history["*"]; global.Until.After(record.Until) {
				record = global
			}
			p.WeeklyRecords[account] = record
		}
	}
	unmapped := map[string]bool{}
	for user := range resetUsers {
		if user != "" {
			if _, ok := enabled[user]; !ok {
				unmapped[user] = true
			}
		}
	}
	for user, ats := range ownClocks {
		if len(ats) > 0 {
			if _, ok := enabled[user]; !ok {
				unmapped[user] = true
			}
		}
	}
	p.UnmappedAccounts = len(unmapped)
	hash := sha256.New()
	for _, raw := range [][]byte{settings, warmup, autoReset, resetsUsed} {
		hash.Write(raw)
		hash.Write([]byte{0})
	}
	p.SourceDigest = hex.EncodeToString(hash.Sum(nil))
	p.Hash = planHash(p)
	return p, nil
}

// ApplyPlan changes only this module's private state. It never enables a timer
// or changes registry/auth files. An identical import preserves the live ledger.
func (s Store) ApplyPlan(ctx context.Context, p ImportPlan, expect string, enable bool, now time.Time) error {
	if expect == "" || expect != p.Hash || planHash(p) != p.Hash {
		return errors.New("maintenance plan changed; inspect a fresh plan")
	}
	if len(p.Accounts) == 0 && enable {
		return errors.New("maintenance has no mapped enabled accounts")
	}
	tx, err := s.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer tx.close()
	if tx.doc.ImportDigest != "" {
		if tx.doc.ImportDigest != p.Hash {
			// A changed, explicitly reviewed snapshot updates only policy.
			// Never replace live attempts, daily markers or a spent-week guard
			// with an older snapshot. Removed policy keys keep their ledger.
			for account := range p.Accounts {
				if tx.doc.Ledger[account] == nil {
					tx.doc.Ledger[account] = importedLedger(p, account, now)
				}
				l := tx.doc.Ledger[account]
				if p.Accounts[account].WeeklyReset {
					record := p.WeeklyRecords[account]
					if p.WeeklyHistoryUnknown {
						record = WeeklyRecord{Until: now.Add(7 * 24 * time.Hour), At: now, Code: "history_unknown"}
					}
					if record.Until.After(l.Weekly.Until) {
						l.Weekly = record
					}
				}
			}
			tx.doc.Adapter = append(json.RawMessage(nil), p.Adapter...)
			tx.doc.Policy.Accounts = p.Accounts
			tx.doc.Policy.Enabled = enable
			if enable {
				tx.doc.Policy.EnabledAt = now.UTC()
			}
			tx.doc.ImportDigest = p.Hash
			return tx.save()
		}
		if tx.doc.Policy.Enabled != enable {
			tx.doc.Policy.Enabled = enable
			if enable {
				tx.doc.Policy.EnabledAt = now.UTC()
			}
			return tx.save()
		}
		return nil
	}
	tx.doc = emptyDocument()
	tx.doc.Adapter = append(json.RawMessage(nil), p.Adapter...)
	tx.doc.Policy.Accounts = p.Accounts
	tx.doc.Policy.Enabled = enable
	if enable {
		tx.doc.Policy.EnabledAt = now.UTC()
	}
	tx.doc.ImportDigest = p.Hash
	tx.doc.LegacyWarmup = append([]byte{}, p.LegacyWarmup...)
	tx.doc.LegacyAutoReset = append([]byte{}, p.LegacyAutoReset...)
	tx.doc.LegacyResetsUsed = append([]byte{}, p.LegacyResetsUsed...)
	for account := range p.Accounts {
		tx.doc.Ledger[account] = importedLedger(p, account, now)
	}
	return tx.save()
}

func importedLedger(p ImportPlan, account string, now time.Time) *AccountLedger {
	windows := p.Windows[account]
	if windows == nil {
		windows = map[string]WarmWindow{}
	}
	l := &AccountLedger{Windows: windows, CompletedCards: map[string]time.Time{}}
	if p.Accounts[account].WeeklyReset {
		l.Weekly = p.WeeklyRecords[account]
		if p.WeeklyHistoryUnknown {
			l.Weekly = WeeklyRecord{Until: now.Add(7 * 24 * time.Hour), At: now, Code: "history_unknown"}
		}
	}
	ambiguous := []string{}
	for name, w := range windows {
		if w.Pending || (w.Failed > 0 && w.Warmed.IsZero()) {
			ambiguous = append(ambiguous, name)
		}
	}
	if len(ambiguous) > 0 {
		sort.Strings(ambiguous)
		l.Warm = &WarmIntent{Phase: "uncertain", Windows: ambiguous, PreparedAt: now.UTC()}
		l.LastResult = "imported_warmup_outcome_unknown"
	}
	return l
}

type Status struct {
	Enabled         bool `json:"enabled"`
	Accounts        int  `json:"accounts"`
	WeeklyWarmup    int  `json:"weekly_warmup"`
	FiveHourWarmup  int  `json:"five_hour_warmup"`
	ScheduledWarmup int  `json:"scheduled_warmup"`
	WeeklyReset     int  `json:"weekly_reset"`
	ExpiringReset   int  `json:"expiring_reset"`
	PendingWarmup   int  `json:"pending_warmup"`
	PendingReset    int  `json:"pending_reset"`
	LocalOnly       bool `json:"local_only"`
}

func (s Store) Status() (Status, error) {
	d, err := s.Read()
	if err != nil {
		return Status{}, err
	}
	out := Status{Enabled: d.Policy.Enabled, Accounts: len(d.Policy.Accounts), LocalOnly: true}
	for _, p := range d.Policy.Accounts {
		if p.WeeklyWarmup {
			out.WeeklyWarmup++
		}
		if p.FiveHourWarmup {
			out.FiveHourWarmup++
		}
		if len(p.WarmAts) > 0 {
			out.ScheduledWarmup++
		}
		if p.WeeklyReset {
			out.WeeklyReset++
		}
		if p.ExpiringReset {
			out.ExpiringReset++
		}
	}
	for _, l := range d.Ledger {
		if l.Warm != nil {
			out.PendingWarmup++
		}
		if l.Reset != nil {
			out.PendingReset++
		}
	}
	return out, nil
}
