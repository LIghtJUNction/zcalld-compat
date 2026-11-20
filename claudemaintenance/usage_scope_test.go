package claudemaintenance

import (
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

func TestNoExtraUsesTheRequestedModelScope(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	for _, item := range []struct {
		scope, model  string
		until         *time.Time
		held, unknown bool
	}{
		{"Haiku", "haiku", &future, true, false},
		{"Claude Haiku 4.5", "claude-haiku-4-5", &future, true, false},
		{"Opus", "haiku", &future, false, false},
		{"Sonnet", "claude-sonnet-4-5", &future, true, false},
		{"", "haiku", &future, true, false},
		{"Haiku", "haiku", &past, false, false},
		{"future family", "haiku", &future, false, true},
		{"Haiku", "custom-alias", &future, false, true},
	} {
		u := usage{Known: true, Windows: []maintenance.Window{{Name: "scope", Used: 100, ResetsAt: item.until, Model: item.scope}}}
		held, err := u.heldForModel(account{NoExtra: true}, item.model, now)
		if held != item.held || (err != nil) != item.unknown {
			t.Fatalf("scope=%q model=%q held=%v err=%v", item.scope, item.model, held, err)
		}
	}
}

func TestStructuredPartialUsageRetainsScopedMeters(t *testing.T) {
	data := []byte(`[{"usage_report":{"rate_limits":{"five_hour":{"utilization":1},"seven_day":null,"seven_day_sonnet":{"utilization":100}}}},{"type":"result","result":"Current week (Haiku): 100% used"}]`)
	u, err := parseUsage(data, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 3 {
		t.Fatalf("scoped readings lost: %+v", u.Windows)
	}
	for _, model := range []string{"haiku", "sonnet"} {
		held, err := u.heldForModel(account{NoExtra: true}, model, time.Now())
		if err != nil || !held {
			t.Fatal(model, held, err)
		}
	}
	if held, err := u.heldForModel(account{NoExtra: true}, "opus", time.Now()); err != nil || held {
		t.Fatal(held, err)
	}
}
