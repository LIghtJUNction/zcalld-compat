// Package maintenance runs optional, bounded Codex maintenance without a resident loop.
// Credentials and account policy remain owned by the existing community host.
package maintenance

import (
	"context"
	"errors"
	"math"
	"regexp"
	"time"
)

const (
	SchemaVersion = 1
	MaxAccounts   = 256
	MaxFileBytes  = 4 << 20
	DefaultBudget = 60 * time.Second
)

var (
	ErrBusy        = errors.New("maintenance is already running")
	ErrInvalid     = errors.New("maintenance state is invalid")
	ErrUnsafe      = errors.New("maintenance path is not private and regular")
	ErrConfigured  = errors.New("maintenance already has a different imported policy")
	accountPattern = regexp.MustCompile(`^builtin:codex#[a-f0-9]{20}$`)
)

type AccountPolicy struct {
	WeeklyWarmup     bool     `json:"weekly_warmup"`
	FiveHourWarmup   bool     `json:"five_hour_warmup,omitempty"`
	WarmAts          []string `json:"warm_ats,omitempty"`
	WeeklyReset      bool     `json:"weekly_reset,omitempty"`
	ExpiringReset    bool     `json:"expiring_reset"`
	ResetLeadMinutes int      `json:"reset_lead_minutes"`
}

type Policy struct {
	Enabled   bool                     `json:"enabled"`
	EnabledAt time.Time                `json:"enabled_at,omitempty"`
	Accounts  map[string]AccountPolicy `json:"accounts"`
}

// WarmWindow uses the source ledger's field names. Import preserves every field,
// including pending/backoff/absent markers, instead of restarting the warmup clock.
type WarmWindow struct {
	ResetsAt time.Time `json:"resetsAt,omitempty"`
	Used     float64   `json:"used"`
	Warmed   time.Time `json:"warmed,omitempty"`
	Pending  bool      `json:"pending,omitempty"`
	Failed   int       `json:"failed,omitempty"`
	Daily    string    `json:"daily,omitempty"`
	Idle     int       `json:"idle,omitempty"`
	Retry    time.Time `json:"retry,omitempty"`
	Absent   bool      `json:"absent,omitempty"`
}

type WarmIntent struct {
	Phase      string            `json:"phase"`
	Windows    []string          `json:"windows"`
	PreparedAt time.Time         `json:"prepared_at"`
	AttemptAt  time.Time         `json:"attempt_at,omitempty"`
	Daily      map[string]string `json:"daily,omitempty"`
}

// WeeklyRecord is kept independently of expiring-card maintenance. The latter
// must not silently use up the once-per-original-week allowance.
type WeeklyRecord struct {
	Until time.Time `json:"until,omitempty"`
	At    time.Time `json:"at,omitempty"`
	Code  string    `json:"code,omitempty"`
}

type WeeklyTrigger string

const (
	WeeklyBeforeCredits   WeeklyTrigger = "before_credits"
	WeeklyRoutesExhausted WeeklyTrigger = "routes_exhausted"
)

type ResetIntent struct {
	Phase           string    `json:"phase"`
	CreditID        string    `json:"credit_id"`
	RedeemRequestID string    `json:"redeem_request_id"`
	ExpiresAt       time.Time `json:"expires_at"`
	PreparedAt      time.Time `json:"prepared_at"`
	AttemptAt       time.Time `json:"attempt_at,omitempty"`
	Purpose         string    `json:"purpose,omitempty"`
	WeekUntil       time.Time `json:"week_until,omitempty"`
}

type AccountLedger struct {
	Windows         map[string]WarmWindow `json:"windows"`
	Warm            *WarmIntent           `json:"warm_intent,omitempty"`
	Reset           *ResetIntent          `json:"reset_intent,omitempty"`
	CompletedCards  map[string]time.Time  `json:"completed_cards,omitempty"`
	NextWarmCheck   time.Time             `json:"next_warm_check,omitempty"`
	LastWarmCheck   time.Time             `json:"last_warm_check,omitempty"`
	NextResetCheck  time.Time             `json:"next_reset_check,omitempty"`
	NextWeeklyCheck time.Time             `json:"next_weekly_check,omitempty"`
	Weekly          WeeklyRecord          `json:"weekly,omitempty"`
	LastResult      string                `json:"last_result,omitempty"`
}

// Document is one atomic private transaction: policy and execution ledger cannot
// be committed in different generations. LegacyWarmup retains exact input bytes.
type Document struct {
	Version          int                       `json:"version"`
	Policy           Policy                    `json:"policy"`
	Ledger           map[string]*AccountLedger `json:"ledger"`
	ImportDigest     string                    `json:"import_digest,omitempty"`
	LegacyWarmup     []byte                    `json:"legacy_warmup,omitempty"`
	LegacyAutoReset  []byte                    `json:"legacy_auto_reset,omitempty"`
	LegacyResetsUsed []byte                    `json:"legacy_resets_used,omitempty"`
}

type Window struct {
	Name        string     `json:"name"`
	Used        float64    `json:"used"`
	SpanSeconds int64      `json:"span_seconds"`
	ResetsAt    *time.Time `json:"resets_at"`
	Aside       bool       `json:"aside"`
	Model       string     `json:"model"`
}

type ResetCard struct {
	ID        string     `json:"id"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type ResetCredits struct {
	Count     int       `json:"count"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Snapshot struct {
	Account    string    `json:"account"`
	Enabled    bool      `json:"enabled"`
	Fresh      bool      `json:"fresh"`
	ObservedAt time.Time `json:"observed_at"`
	NoCredits  bool      `json:"no_credits"`
	// OnCredits follows the source's CodexGoesOnCredits rule: an explicitly
	// unlimited account, or has_credits with a positive/unparseable balance.
	// A missing/failed credits object is never inferred to authorize a spend.
	OnCredits   bool `json:"on_credits"`
	UsageKnown  bool `json:"usage_known"`
	ModelsKnown bool `json:"models_known"`
	// Held is the trusted host's current account-wide hold. The builtin loader
	// independently rechecks the model-specific NoCredits guard before sending.
	Held          bool          `json:"held"`
	Windows       []Window      `json:"windows"`
	CardsComplete bool          `json:"cards_complete"`
	Cards         []ResetCard   `json:"reset_cards"`
	ResetCredits  *ResetCredits `json:"reset_credits,omitempty"`
}

type ConsumeRequest struct {
	CreditID          string        `json:"credit_id"`
	RedeemRequestID   string        `json:"redeem_request_id"`
	ExpectedExpiresAt time.Time     `json:"expected_expires_at"`
	LeadMinutes       int           `json:"lead_minutes"`
	Purpose           string        `json:"purpose,omitempty"`
	Trigger           WeeklyTrigger `json:"trigger,omitempty"`
	ExpectedWeekUntil time.Time     `json:"expected_week_until,omitempty"`
}

type ActionResult struct {
	// completed: a bounded successful response was read. not_sent: trusted host
	// proves no request was sent. unknown: it may have been sent; never guess.
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// Host must validate the explicit opaque account against the current registry,
// retain the existing credential-owner and NoCredits rules, and cooperate with
// the single context deadline including credential drain. It must never select
// a fallback account. Warmup has no caller-controlled URL, model or prompt.
type Host interface {
	Inspect(context.Context, string) (Snapshot, error)
	Warmup(context.Context, string, []string) (ActionResult, error)
	Consume(context.Context, string, ConsumeRequest) (ActionResult, error)
}

type AccountResult struct {
	Account string `json:"account"`
	Action  string `json:"action"`
	Status  string `json:"status"`
}

type Report struct {
	Enabled bool            `json:"enabled"`
	Status  string          `json:"status"`
	Results []AccountResult `json:"results"`
}

func emptyDocument() Document {
	return Document{Version: SchemaVersion, Policy: Policy{Accounts: map[string]AccountPolicy{}}, Ledger: map[string]*AccountLedger{}}
}

func validSnapshot(s Snapshot, account string) bool {
	if s.Account != account || !s.Enabled || len(s.Windows) > 64 || len(s.Cards) > 128 {
		return false
	}
	windows := map[string]bool{}
	for _, w := range s.Windows {
		if w.Name == "" || len(w.Name) > 120 || windows[w.Name] || math.IsNaN(w.Used) || math.IsInf(w.Used, 0) || w.Used < 0 || w.Used > 100 || w.SpanSeconds < 0 || w.SpanSeconds > 366*86400 {
			return false
		}
		windows[w.Name] = true
	}
	cards := map[string]bool{}
	for _, c := range s.Cards {
		if c.ID == "" || len(c.ID) > 512 || cards[c.ID] {
			return false
		}
		cards[c.ID] = true
	}
	return true
}

func freshSnapshot(s Snapshot, account string, now time.Time) bool {
	return validSnapshot(s, account) && s.Fresh && !s.ObservedAt.IsZero() &&
		!s.ObservedAt.After(now.Add(5*time.Second)) && now.Sub(s.ObservedAt) <= time.Minute
}
