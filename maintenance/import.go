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
	Accounts         map[string]AccountPolicy         `json:"accounts"`
	Windows          map[string]map[string]WarmWindow `json:"windows"`
	LegacyWarmup     []byte                           `json:"legacy_warmup"`
	SourceDigest     string                           `json:"source_digest"`
	UnmappedAccounts int                              `json:"unmapped_accounts"`
	Hash             string                           `json:"hash"`
}

type legacySettings struct {
	Warmup        string            `json:"codexWarmup"`
	WarmAts       []string          `json:"codexWarmAts"`
	WarmAtOf      map[string]string `json:"codexWarmAtOf"`
	AutoReset     []string          `json:"codexAutoReset"`
	AutoResetOnly map[string]string `json:"codexAutoResetOnly"`
	ResetLead     int               `json:"codexResetLead"`
}

func planHash(p ImportPlan) string {
	p.Hash = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PlanImport is local-only. Unsupported five-hour/daily/weekly-reset schedules
// fail explicitly; they are never silently broadened or enabled.
func PlanImport(settings, warmup []byte, links []AccountLink) (ImportPlan, error) {
	p := ImportPlan{Accounts: map[string]AccountPolicy{}, Windows: map[string]map[string]WarmWindow{}, LegacyWarmup: append([]byte{}, warmup...)}
	if len(settings) > 2<<20 || len(warmup) > 2<<20 || len(links) > MaxAccounts {
		return p, ErrInvalid
	}
	var s legacySettings
	if json.Unmarshal(settings, &s) != nil {
		return p, ErrInvalid
	}
	if s.Warmup != "" && s.Warmup != "off" && s.Warmup != "week" {
		return p, errors.New("only the verified weekly warmup policy can be imported")
	}
	if len(s.WarmAts) > 0 {
		return p, errors.New("scheduled warmup times are not supported by this maintenance module")
	}
	for _, at := range s.WarmAtOf {
		if at != "" && at != "off" {
			return p, errors.New("per-account warmup times are not supported")
		}
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
	for user, account := range enabled {
		reset := resetUsers[user]
		if reset && s.AutoResetOnly[user] != "expiring" {
			return p, errors.New("only the verified expiring-reset policy can be imported")
		}
		policy := AccountPolicy{WeeklyWarmup: s.Warmup == "week", ExpiringReset: reset, ResetLeadMinutes: lead}
		if !policy.WeeklyWarmup && !policy.ExpiringReset {
			continue
		}
		p.Accounts[account] = policy
		ws := legacy[user]
		if ws == nil {
			ws = map[string]WarmWindow{}
		}
		p.Windows[account] = ws
	}
	for user := range resetUsers {
		if _, ok := enabled[user]; !ok {
			p.UnmappedAccounts++
		}
	}
	hash := sha256.New()
	hash.Write(settings)
	hash.Write([]byte{0})
	hash.Write(warmup)
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
			return ErrConfigured
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
	tx.doc.Policy.Accounts = p.Accounts
	tx.doc.Policy.Enabled = enable
	if enable {
		tx.doc.Policy.EnabledAt = now.UTC()
	}
	tx.doc.ImportDigest = p.Hash
	tx.doc.LegacyWarmup = append([]byte{}, p.LegacyWarmup...)
	for account, windows := range p.Windows {
		l := &AccountLedger{Windows: windows, CompletedCards: map[string]time.Time{}}
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
		tx.doc.Ledger[account] = l
	}
	return tx.save()
}

type Status struct {
	Enabled       bool `json:"enabled"`
	Accounts      int  `json:"accounts"`
	WeeklyWarmup  int  `json:"weekly_warmup"`
	ExpiringReset int  `json:"expiring_reset"`
	PendingWarmup int  `json:"pending_warmup"`
	PendingReset  int  `json:"pending_reset"`
	LocalOnly     bool `json:"local_only"`
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
