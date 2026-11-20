package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
)

type quotaEventWindow struct {
	ID          string  `json:"id"`
	Pool        string  `json:"pool"`
	UsedPercent float64 `json:"used_percent"`
	SpanSeconds int64   `json:"span_seconds"`
	ResetAtMS   *int64  `json:"reset_at_ms"`
	Aside       bool    `json:"aside"`
}
type resetCreditEvent struct {
	Count       int   `json:"count"`
	ExpiresAtMS int64 `json:"expires_at_ms"`
}
type quotaEvent struct {
	Version       int                `json:"version"`
	SourceID      string             `json:"source_id"`
	ObservationID string             `json:"observation_id"`
	ObservedAtMS  int64              `json:"observed_at_ms"`
	Fresh         bool               `json:"fresh"`
	Success       bool               `json:"success"`
	Windows       []quotaEventWindow `json:"windows"`
	Balance       any                `json:"balance"`
	ResetCredits  *resetCreditEvent  `json:"reset_credits,omitempty"`
}

// DesktopObserver uses only a fresh inspection already obtained by Run. It
// never adds a metadata request or guesses a currency for a credit balance.
// Engine deliberately ignores publication errors so notification delivery
// cannot turn a completed primary action into an apparent failure.
func DesktopObserver(inbox string) func(Snapshot) error {
	return func(s Snapshot) error {
		if inbox == "" {
			return nil
		}
		event, err := snapshotEvent(s, time.Now().UTC())
		if err != nil {
			return err
		}
		return desktop.Publish(inbox, 1, event)
	}
}
func snapshotEvent(s Snapshot, now time.Time) (quotaEvent, error) {
	event := quotaEvent{}
	if !validSnapshot(s, s.Account) || !accountPattern.MatchString(s.Account) || !s.Fresh || !s.UsageKnown ||
		s.ObservedAt.IsZero() || s.ObservedAt.After(now.Add(5*time.Second)) || now.Sub(s.ObservedAt) > time.Minute {
		return event, errors.New("desktop quota observation is not fresh and proven")
	}
	source := sha256.Sum256([]byte("zcalld:codex-account:v1\x00" + s.Account))
	event = quotaEvent{Version: 1, SourceID: hex.EncodeToString(source[:]), ObservedAtMS: s.ObservedAt.UnixMilli(), Fresh: true, Success: true, Windows: []quotaEventWindow{}}
	for _, w := range s.Windows {
		if w.Aside || w.Model != "" || (w.Name != "primary" && w.Name != "secondary") {
			continue
		}
		var reset *int64
		if w.ResetsAt != nil && !w.ResetsAt.IsZero() {
			n := w.ResetsAt.UnixMilli()
			reset = &n
		}
		event.Windows = append(event.Windows, quotaEventWindow{w.Name, "account", w.Used, w.SpanSeconds, reset, false})
	}
	sort.Slice(event.Windows, func(i, j int) bool { return event.Windows[i].ID < event.Windows[j].ID })
	// CardsComplete means the fixed credits GET and usage count agreed. Check
	// the matching count and minimum expiry again before emitting its distinct
	// semantic field; this is never a quota-window reset time.
	if r := s.ResetCredits; r != nil && s.CardsComplete && r.Count > 0 && r.Count == len(s.Cards) && !r.ExpiresAt.IsZero() {
		var earliest time.Time
		for _, c := range s.Cards {
			if c.ExpiresAt != nil && !c.ExpiresAt.IsZero() && (earliest.IsZero() || c.ExpiresAt.Before(earliest)) {
				earliest = *c.ExpiresAt
			}
		}
		if earliest.Equal(r.ExpiresAt) {
			event.ResetCredits = &resetCreditEvent{r.Count, r.ExpiresAt.UnixMilli()}
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return quotaEvent{}, err
	}
	id := sha256.Sum256(data)
	event.ObservationID = hex.EncodeToString(id[:])
	return event, nil
}
