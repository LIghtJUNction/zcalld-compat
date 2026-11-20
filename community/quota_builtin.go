package community

import (
	"encoding/json"
	"errors"
	"math"
	"time"
)

type nativeQuotaError string

func (e nativeQuotaError) Error() string { return string(e) }
func nativeQuotaCode(code string) string {
	switch code {
	case "native_auth_unavailable", "account_or_plugin_changed", "timeout", "provider_error":
		return code
	default:
		return "provider_error"
	}
}

func callBuiltinQuota(root string, entry Entry, account string, deadline time.Time) (quotaReading, error) {
	// Metadata never renews credentials. Keep the existing bounded close contract
	// for cancellation/worker cleanup without extending the operation deadline.
	registry, err := Read(root)
	if err != nil {
		return quotaReading{}, err
	}
	current, reason := verifiedBuiltinCodex(root, registry)
	if reason != "" || !sameHostEntry(current, entry) {
		return quotaReading{}, nativeQuotaError("account_or_plugin_changed")
	}
	host, err := startBuiltinCodexHost(root, deadline, deadline.Add(35*time.Second))
	if err != nil {
		return quotaReading{}, err
	}
	defer host.Close()
	var result struct {
		OK      bool          `json:"ok"`
		Error   string        `json:"error"`
		At      string        `json:"at"`
		Windows []quotaWindow `json:"windows"`
	}
	if err := host.callUntil("codex.usage", map[string]any{"account": account, "deadline_ms": deadline.UnixMilli()}, &result, deadline); err != nil {
		return quotaReading{}, err
	}
	if !result.OK {
		return quotaReading{}, nativeQuotaError(nativeQuotaCode(result.Error))
	}
	at, err := time.Parse(time.RFC3339Nano, result.At)
	if err != nil || time.Since(at) > 2*time.Minute || at.After(time.Now().Add(10*time.Second)) || len(result.Windows) < 1 || len(result.Windows) > 2 {
		return quotaReading{}, errors.New("invalid builtin quota result")
	}
	seen := map[string]bool{}
	for _, window := range result.Windows {
		if (window.Name != "primary" && window.Name != "secondary") || seen[window.Name] ||
			window.Used == nil || quotaNumber(window.Used) == nil || *window.Used > 100 ||
			math.IsNaN(window.SpanSeconds) || math.IsInf(window.SpanSeconds, 0) || math.Trunc(window.SpanSeconds) != window.SpanSeconds || window.SpanSeconds < 1 || window.SpanSeconds > 366*86400 {
			return quotaReading{}, errors.New("invalid builtin quota window")
		}
		seen[window.Name] = true
	}
	return quotaReading{At: at.UTC().Format(time.RFC3339Nano), Windows: result.Windows, codex: true}, nil
}

func sameQuotaAuth(before, after any) bool {
	a, e := json.Marshal(before)
	if e != nil {
		return false
	}
	b, e := json.Marshal(after)
	return e == nil && string(a) == string(b)
}
