package community

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
)

type quotaDesktopWindow struct {
	ID          string  `json:"id"`
	Pool        string  `json:"pool,omitempty"`
	Used        float64 `json:"used_percent"`
	SpanSeconds float64 `json:"span_seconds,omitempty"`
	ResetAt     *int64  `json:"reset_at_ms,omitempty"`
	Aside       bool    `json:"aside,omitempty"`
}
type quotaDesktopBalance struct {
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit"`
}
type quotaDesktopEvent struct {
	Version       int                  `json:"version"`
	SourceID      string               `json:"source_id"`
	ObservationID string               `json:"observation_id"`
	ObservedAt    int64                `json:"observed_at_ms"`
	Fresh         bool                 `json:"fresh"`
	Success       bool                 `json:"success"`
	Windows       []quotaDesktopWindow `json:"windows"`
	Balance       *quotaDesktopBalance `json:"balance,omitempty"`
}

func quotaDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Units are explicit provider declarations; symbols such as "$" do not prove
// a currency. Credits remain credits and are never interpreted as money.
func quotaBalanceUnit(unit string) bool {
	switch unit {
	case "USD", "EUR", "GBP", "CNY", "JPY", "CAD", "AUD", "CHF", "HKD", "SGD", "KRW", "INR", "credits", "tokens", "requests":
		return true
	default:
		return false
	}
}

// Only this invocation's successfully persisted reading may reach this mapper.
// ResetSecs is a countdown, not a period. Generic hooks do not attest a period
// length, so these events cannot qualify for renewal reminders.
func quotaDesktopObservation(account string, reading quotaReading, now time.Time) (quotaDesktopEvent, bool) {
	at, err := time.Parse(time.RFC3339Nano, reading.At)
	if err != nil || at.Before(now.Add(-2*time.Minute)) || at.After(now.Add(10*time.Second)) || len(reading.Windows) > 64 {
		return quotaDesktopEvent{}, false
	}
	event := quotaDesktopEvent{Version: 1, SourceID: quotaDigest("zcalld:quota-account:v1\x00" + account),
		ObservedAt: at.UnixMilli(), Fresh: true, Success: true, Windows: []quotaDesktopWindow{}}
	if reading.codex {
		event.SourceID = quotaDigest("zcalld:codex-account:v1\x00" + account)
	}
	identities := map[string]int{}
	for _, w := range reading.Windows {
		if w.Name != "" {
			identities[w.Name+"\x00"+w.Unit]++
		}
	}
	balances := 0
	for _, w := range reading.Windows {
		if value := quotaNumber(w.Balance); value != nil && quotaBalanceUnit(w.Unit) {
			balances++
			event.Balance = &quotaDesktopBalance{*value, w.Unit}
		}
		identity := w.Name + "\x00" + w.Unit
		used := quotaNumber(w.Used)
		if w.Name == "" || identities[identity] != 1 || used == nil || *used > 100 {
			continue
		}
		item := quotaDesktopWindow{ID: quotaDigest("zcalld:quota-window:v1\x00" + identity), Used: *used}
		if reading.codex {
			item.ID = w.Name
			item.Pool = "account"
			item.SpanSeconds = w.SpanSeconds
		}
		if reset, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			// A window with an already elapsed reset is not a current threshold
			// observation. Unknown reset remains usable for usage thresholds only.
			if !reset.After(now) {
				continue
			}
			if reset.Before(now.Add(366 * 24 * time.Hour)) {
				value := reset.UnixMilli()
				item.ResetAt = &value
			}
		}
		event.Windows = append(event.Windows, item)
	}
	// A single aggregate balance cannot safely combine different meters,
	// currencies or duplicate provider windows.
	if balances != 1 {
		event.Balance = nil
	}
	if len(event.Windows) == 0 && event.Balance == nil {
		return quotaDesktopEvent{}, false
	}
	sort.Slice(event.Windows, func(i, j int) bool { return event.Windows[i].ID < event.Windows[j].ID })
	raw, err := json.Marshal(event)
	if err != nil {
		return quotaDesktopEvent{}, false
	}
	event.ObservationID = quotaDigest(string(raw))
	return event, true
}

func publishQuotaDesktop(root, account string, reading quotaReading) error {
	config, err := desktop.ReadConfig(root)
	if err != nil {
		return errors.New("desktop quota notification settings are unavailable")
	}
	if !config.QuotaEnabled() {
		return nil
	}
	event, ok := quotaDesktopObservation(account, reading, time.Now())
	if !ok {
		return nil
	}
	if err := desktop.Publish(config.Inbox, 1, event); err != nil {
		return errors.New("desktop quota notification was not published")
	}
	return nil
}
