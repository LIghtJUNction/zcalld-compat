package community

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

func quotaDesktopFixture(t *testing.T, enabled bool) (string, string) {
	t.Helper()
	original, _ := quotaFixture(t)
	root, err := filepath.EvalSymlinks(original)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(root)
	if err := privatefile.Protect(parent, 0700); err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(parent, "desktop", "inbox")
	if err := os.MkdirAll(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Protect(queue, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"enabled": enabled, "usage_alert": 85, "balance_alert": 5, "inbox": queue})
	if err := Atomic(filepath.Join(parent, "desktop.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root, queue
}
func quotaEvents(t *testing.T, queue string) []quotaDesktopEvent {
	t.Helper()
	entries, err := os.ReadDir(queue)
	if err != nil {
		t.Fatal(err)
	}
	result := []quotaDesktopEvent{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(queue, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "quota#one") ||
			strings.Contains(string(raw), "quota-fixture") {
			t.Fatal("private metadata entered desktop inbox")
		}
		var envelope struct {
			Kind  uint32            `json:"kind"`
			Event quotaDesktopEvent `json:"event"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Kind != 1 {
			t.Fatal("invalid event envelope")
		}
		result = append(result, envelope.Event)
	}
	return result
}
func freshQuotaReading() quotaReading {
	used, balance := 88.0, 2.5
	return quotaReading{At: time.Now().UTC().Format(time.RFC3339Nano), Windows: []quotaWindow{
		{Name: "PRIVATE-METER", Used: &used, ResetSecs: 86400, ResetsAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
		{Name: "PRIVATE-BALANCE", Balance: &balance, Unit: "USD"},
	}}
}

func TestQuotaDesktopAfterPersistAndListDoesNotPublish(t *testing.T) {
	root, queue := quotaDesktopFixture(t, true)
	if _, e := QuotaList(root); e != nil {
		t.Fatal(e)
	}
	if len(quotaEvents(t, queue)) != 0 {
		t.Fatal("list published")
	}
	reading := freshQuotaReading()
	value, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) { return reading, nil })
	if e != nil || value["ok"] != true {
		t.Fatal(value, e)
	}
	cache, e := readQuotaCache(root)
	if e != nil || cache.Accounts["quota#one"].At != reading.At {
		t.Fatal("reading not committed")
	}
	events := quotaEvents(t, queue)
	if len(events) != 1 {
		t.Fatal("event count", len(events))
	}
	event := events[0]
	if !event.Fresh || !event.Success || event.Version != 1 || len(event.SourceID) != 64 || len(event.ObservationID) != 64 ||
		len(event.Windows) != 1 || event.Windows[0].Used != 88 || event.Windows[0].SpanSeconds != 0 ||
		event.Windows[0].ResetAt == nil || event.Balance == nil || event.Balance.Amount != 2.5 || event.Balance.Unit != "USD" {
		t.Fatal(event)
	}
	if _, e := QuotaList(root); e != nil {
		t.Fatal(e)
	}
	if len(quotaEvents(t, queue)) != 1 {
		t.Fatal("cached list published a fresh event")
	}
}

func TestQuotaDesktopDisabledFailuresAndCacheFailureDoNotPublish(t *testing.T) {
	for _, kind := range []string{"disabled", "provider", "cache"} {
		t.Run(kind, func(t *testing.T) {
			root, queue := quotaDesktopFixture(t, kind != "disabled")
			value, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) {
				if kind == "provider" {
					return quotaReading{}, errors.New("PRIVATE provider error")
				}
				if kind == "cache" {
					if err := os.Mkdir(filepath.Join(root, quotaCacheFile), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return freshQuotaReading(), nil
			})
			if kind == "disabled" && (e != nil || value["ok"] != true) {
				t.Fatal(value, e)
			}
			if kind == "provider" && (e != nil || value["ok"] != false) {
				t.Fatal(value, e)
			}
			if kind == "cache" && e == nil {
				t.Fatal("cache failure not detected")
			}
			if len(quotaEvents(t, queue)) != 0 {
				t.Fatal("unsuccessful/disabled update published")
			}
		})
	}
}

func TestQuotaDesktopFailureKeepsSuccessfulCache(t *testing.T) {
	for _, kind := range []string{"full", "bad-settings"} {
		t.Run(kind, func(t *testing.T) {
			root, queue := quotaDesktopFixture(t, true)
			if kind == "full" {
				for i := 0; i < desktop.MaxEvents; i++ {
					if e := Atomic(filepath.Join(queue, fmt.Sprintf(".pending-%d", i)), []byte("{}"), 0600); e != nil {
						t.Fatal(e)
					}
				}
			} else {
				if e := Atomic(filepath.Join(filepath.Dir(root), "desktop.json"), []byte(`{"enabled":true,"inbox":"PRIVATE-invalid"}`), 0600); e != nil {
					t.Fatal(e)
				}
			}
			value, e := quotaRefresh(root, "quota#one", func(string, Entry, string, string) (quotaReading, error) { return freshQuotaReading(), nil })
			if e != nil || value["ok"] != true || value["desktop_warning"] == nil {
				t.Fatal(value, e)
			}
			raw, _ := json.Marshal(value)
			if strings.Contains(string(raw), "PRIVATE-invalid") {
				t.Fatal("raw configuration escaped")
			}
			cache, e := readQuotaCache(root)
			if e != nil || len(cache.Accounts) != 1 {
				t.Fatal("notification failure lost cache", e)
			}
			if len(quotaEvents(t, queue)) != 0 {
				t.Fatal("unexpected published event")
			}
		})
	}
}

func TestQuotaDesktopUnknownUnitsDuplicateMetersAndStaleness(t *testing.T) {
	now := time.Now()
	reading := freshQuotaReading()
	first, ok := quotaDesktopObservation("quota#one", reading, now)
	if !ok {
		t.Fatal("valid reading rejected")
	}
	second, ok := quotaDesktopObservation("quota#two", reading, now)
	if !ok || first.SourceID == second.SourceID || first.Windows[0].ID != second.Windows[0].ID {
		t.Fatal("identity is not account scoped and stable")
	}
	reading.Windows[1].Unit = "$"
	event, ok := quotaDesktopObservation("quota#one", reading, now)
	if !ok || event.Balance != nil {
		t.Fatal("ambiguous currency accepted")
	}
	reading.Windows = append(reading.Windows, reading.Windows[0])
	if _, ok := quotaDesktopObservation("quota#one", reading, now); ok {
		t.Fatal("duplicate meter merged")
	}
	reading = freshQuotaReading()
	reading.Windows[0].ResetsAt = now.Add(-time.Second).Format(time.RFC3339Nano)
	event, ok = quotaDesktopObservation("quota#one", reading, now)
	if !ok || len(event.Windows) != 0 {
		t.Fatal("expired window emitted")
	}
	for _, at := range []string{"invalid", now.Add(-3 * time.Minute).Format(time.RFC3339Nano), now.Add(time.Minute).Format(time.RFC3339Nano)} {
		reading.At = at
		if _, ok := quotaDesktopObservation("quota#one", reading, now); ok {
			t.Fatal("non-fresh reading accepted")
		}
	}
	reading = freshQuotaReading()
	reading.Windows = append(reading.Windows, reading.Windows[1])
	event, ok = quotaDesktopObservation("quota#one", reading, now)
	if !ok || event.Balance != nil {
		t.Fatal("multiple balances guessed")
	}
}
