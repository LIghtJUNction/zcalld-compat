package maintenance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDesktopObservationHasStableOpaqueIdentityAndSeparateCreditExpiry(t *testing.T) {
	s := snap(moment)
	s.Windows[0].Name = "secondary"
	s.Windows[0].Used = 12
	expiry := moment.Add(time.Hour)
	s.Cards = []ResetCard{{ID: "card-canary", ExpiresAt: &expiry}}
	s.ResetCredits = &ResetCredits{1, expiry}
	event, err := snapshotEvent(s, moment)
	if err != nil {
		t.Fatal(err)
	}
	again, err := snapshotEvent(s, moment.Add(time.Second))
	if err != nil || event.ObservationID != again.ObservationID {
		t.Fatal("same observation did not deduplicate", err)
	}
	if len(event.SourceID) != 64 || len(event.ObservationID) != 64 || event.ResetCredits == nil || event.ResetCredits.ExpiresAtMS == *event.Windows[0].ResetAtMS {
		t.Fatal("credit expiry confused with quota window")
	}
	b, _ := json.Marshal(event)
	for _, canary := range []string{account, "card-canary"} {
		if strings.Contains(string(b), canary) {
			t.Fatal("private identity leaked")
		}
	}
	if event.Balance != nil {
		t.Fatal("invented currency")
	}
	s.CardsComplete = false
	event, err = snapshotEvent(s, moment)
	if err != nil || event.ResetCredits != nil {
		t.Fatal("unproved credit count published")
	}
	s.CardsComplete = true
	s.ResetCredits.Count = 2
	event, _ = snapshotEvent(s, moment)
	if event.ResetCredits != nil {
		t.Fatal("mismatched count published")
	}
}
func TestDesktopRejectsStaleOrUnknownReadings(t *testing.T) {
	for _, s := range []Snapshot{func() Snapshot { v := snap(moment); v.UsageKnown = false; return v }(), func() Snapshot { v := snap(moment); v.ObservedAt = moment.Add(-2 * time.Minute); return v }(), func() Snapshot { v := snap(moment); v.Fresh = false; return v }()} {
		if _, err := snapshotEvent(s, moment); err == nil {
			t.Fatal("unproved observation published")
		}
	}
}
