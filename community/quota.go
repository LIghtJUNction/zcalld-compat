package community

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const quotaCacheFile = "quota-cache.json"
const quotaTimeout = 30 * time.Second

var quotaNPM = regexp.MustCompile(`^(?:@[a-z0-9._-]+/)?[a-z0-9._-]+(?:@[A-Za-z0-9.+_~^*-]+)?$`)

type quotaCapability struct {
	ID    string `json:"id"`
	Spec  string `json:"spec"`
	Usage bool   `json:"usage"`
}
type quotaWindow struct {
	Name        string   `json:"name"`
	Used        *float64 `json:"used"`
	ResetsAt    string   `json:"resetsAt,omitempty"`
	ResetSecs   float64  `json:"resetSecs,omitempty"`
	SpanSeconds float64  `json:"span_seconds,omitempty"`
	Amount      *float64 `json:"amount,omitempty"`
	Limit       *float64 `json:"limit,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Balance     *float64 `json:"balance,omitempty"`
}
type quotaReading struct {
	At      string        `json:"at"`
	Windows []quotaWindow `json:"windows"`
	codex   bool
}
type quotaCache struct {
	Version  int                     `json:"version"`
	Accounts map[string]quotaReading `json:"accounts"`
}

func quotaText(value string, limit int) string {
	var out strings.Builder
	for _, c := range value {
		if unicode.IsControl(c) || !unicode.IsPrint(c) {
			continue
		}
		if out.Len()+len(string(c)) > limit {
			break
		}
		out.WriteRune(c)
	}
	return out.String()
}
func quotaTime(value string) string {
	t, e := time.Parse(time.RFC3339Nano, value)
	if e != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func quotaNumber(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1<<53 {
		return nil
	}
	n := *value
	return &n
}

func readQuotaCache(root string) (quotaCache, error) {
	cache := quotaCache{Version: 1, Accounts: map[string]quotaReading{}}
	b, e := readLimit(filepath.Join(root, quotaCacheFile), 4<<20)
	if errors.Is(e, os.ErrNotExist) {
		return cache, nil
	}
	if e != nil {
		return cache, errors.New("quota cache is unreadable")
	}
	if json.Unmarshal(b, &cache) != nil || cache.Version != 1 || cache.Accounts == nil || len(cache.Accounts) > 4096 {
		return cache, errors.New("quota cache is invalid")
	}
	return cache, nil
}

// Imported provider metadata remains untouched. A later explicit providers
// discovery can write the smaller capability cache without copying credentials.
func quotaCapabilities(root string) map[string]quotaCapability {
	out := map[string]quotaCapability{}
	for _, name := range []string{"plugin-providers.json", "quota-capabilities.json"} {
		b, e := readLimit(filepath.Join(root, name), 8<<20)
		if e != nil {
			continue
		}
		var value any
		if json.Unmarshal(b, &value) != nil {
			continue
		}
		var rows []any
		switch v := value.(type) {
		case []any:
			rows = v
		case map[string]any:
			if p, ok := v["providers"].([]any); ok {
				rows = p
			} else {
				for id, item := range v {
					if row, ok := item.(map[string]any); ok {
						if _, exists := row["id"]; !exists {
							row["id"] = id
						}
						rows = append(rows, row)
					}
				}
			}
		}
		for _, item := range rows {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := row["id"].(string)
			spec, _ := row["spec"].(string)
			usage, known := row["usage"].(bool)
			if known && providerPattern.MatchString(id) && len(spec) <= 1024 {
				out[id] = quotaCapability{id, spec, usage}
			}
		}
	}
	return out
}
func saveQuotaCapabilities(root string, providers []map[string]any) error {
	rows := []quotaCapability{}
	for _, p := range providers {
		id, _ := p["id"].(string)
		spec, _ := p["spec"].(string)
		usage, ok := p["usage"].(bool)
		if ok && providerPattern.MatchString(id) {
			rows = append(rows, quotaCapability{id, spec, usage})
		}
	}
	b, e := json.Marshal(map[string]any{"version": 1, "providers": rows})
	if e != nil {
		return e
	}
	return Atomic(filepath.Join(root, "quota-capabilities.json"), append(b, '\n'), 0600)
}

func quotaEntry(r Registry, capability quotaCapability) (Entry, string) {
	if !capability.Usage {
		return Entry{}, "unsupported"
	}
	want := capability.Spec
	if prefer := r.Prefer[capability.ID]; prefer != "" {
		want = prefer
	}
	if want == "" {
		return Entry{}, "capability_unknown"
	}
	var matches []Entry
	for _, p := range r.Plugins {
		if p.Kind != "community" {
			continue
		}
		match := p.Source == want
		// Import pins the installed version. The cached original spec can be
		// unversioned, but it must name the same explicit npm package uniquely.
		unprefixed := strings.TrimPrefix(want, "npm:")
		if !match && strings.HasPrefix(p.Source, "npm:") && quotaNPM.MatchString(unprefixed) {
			match = npmName(strings.TrimPrefix(p.Source, "npm:")) == npmName(unprefixed)
		}
		if match {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return Entry{}, "plugin_unavailable"
	}
	if !matches[0].Enabled {
		return Entry{}, "plugin_disabled"
	}
	return matches[0], ""
}

func quotaAccount(root string, r Registry, all map[string]any, caps map[string]quotaCapability, id string) (Entry, string) {
	auth, ok := all[id].(map[string]any)
	provider := accountProvider(id)
	if !ok || len(auth) == 0 || !validAccount(id, provider) {
		return Entry{}, "account_unavailable"
	}
	if !r.Accounts[id].Enabled {
		return Entry{}, "account_disabled"
	}
	if provider == "builtin:codex" {
		if auth["type"] != "oauth" {
			return Entry{}, "account_unavailable"
		}
		// Explicit native accounts may not be enabled solely by in-memory legacy
		// defaults. The host rechecks this same private registry before its GET.
		actual, err := Read(root)
		if err != nil {
			return Entry{}, "account_unavailable"
		}
		meta, exists := actual.Accounts[id]
		if !exists || !meta.Enabled {
			return Entry{}, "account_disabled"
		}
		return verifiedBuiltinCodex(root, actual)
	}
	c, known := caps[provider]
	if !known {
		return Entry{}, "capability_unknown"
	}
	p, reason := quotaEntry(r, c)
	if reason != "" {
		return p, reason
	}
	// Arbitrary community hooks cannot be assumed to understand an external
	// refresh owner. No such hook may take over its sign-in during quota reads.
	if auth["refreshOwner"] == "external" {
		return Entry{}, "external_owner"
	}
	return p, ""
}

func QuotaList(root string) (map[string]any, error) {
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	all, _, e := readAuthFile(root)
	if e != nil {
		return nil, e
	}
	seedAccounts(root, &r, all) // in-memory defaults only, no registry/auth mutation
	caps := quotaCapabilities(root)
	cache, e := readQuotaCache(root)
	if e != nil {
		return nil, e
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	accounts := []any{}
	readings := []any{}
	for _, id := range ids {
		_, reason := quotaAccount(root, r, all, caps, id)
		label := quotaText(r.Accounts[id].Label, 180)
		if label == "" {
			label = quotaText(id, 180)
		}
		reading, exists := cache.Accounts[id]
		accounts = append(accounts, map[string]any{"id": id, "provider": accountProvider(id), "label": label,
			"enabled": r.Accounts[id].Enabled, "can_refresh": reason == "", "reason": reason, "cached_at": reading.At})
		if exists {
			readings = append(readings, map[string]any{"account": id, "provider": accountProvider(id),
				"label": label, "at": reading.At, "windows": reading.Windows})
		}
	}
	return map[string]any{"schema_version": 1, "local_only": true, "accounts": accounts, "readings": readings}, nil
}

type quotaCaller func(root string, entry Entry, provider, account string) (quotaReading, error)

func callQuota(root string, entry Entry, provider, account string) (quotaReading, error) {
	deadline := time.Now().Add(quotaTimeout)
	if provider == "builtin:codex" {
		return callBuiltinQuota(root, entry, account, deadline)
	}
	h, e := startHost(root, entry.Source, deadline)
	if e != nil {
		return quotaReading{}, e
	}
	defer h.Close()
	var raw struct {
		Error   string        `json:"error"`
		SignIn  string        `json:"signIn"`
		Windows []quotaWindow `json:"windows"`
	}
	e = h.callUntil("usage", map[string]any{"provider": provider, "account": account}, &raw, deadline)
	if e != nil {
		return quotaReading{}, e
	}
	if raw.Error != "" || raw.SignIn == "expired" || len(raw.Windows) > 64 {
		return quotaReading{}, errors.New("quota provider returned an unusable result")
	}
	reading := quotaReading{At: time.Now().UTC().Format(time.RFC3339Nano), Windows: []quotaWindow{}}
	for _, w := range raw.Windows {
		w.Name = quotaText(w.Name, 120)
		w.Unit = quotaText(w.Unit, 40)
		w.ResetsAt = quotaTime(w.ResetsAt)
		w.Used = quotaNumber(w.Used)
		if w.Used != nil && *w.Used > 100 {
			w.Used = nil
		}
		w.Amount = quotaNumber(w.Amount)
		w.Limit = quotaNumber(w.Limit)
		w.Balance = quotaNumber(w.Balance)
		if w.Limit == nil || *w.Limit == 0 {
			w.Amount = nil
			w.Limit = nil
		}
		if math.IsNaN(w.ResetSecs) || math.IsInf(w.ResetSecs, 0) || w.ResetSecs < 0 || w.ResetSecs > 366*86400 {
			w.ResetSecs = 0
		}
		reading.Windows = append(reading.Windows, w)
	}
	return reading, nil
}

func quotaFailure(code string) map[string]any {
	message := "Quota update was not completed; previous cached readings were kept."
	if code == "builtin_runtime_outdated" {
		message = "Run zcalld plugins runtime configure for this configuration to activate the installed builtin adapters; saved accounts and credentials are preserved."
	}
	if code == "native_auth_unavailable" {
		message = "The native Codex sign-in is expired, unavailable, or changed. Renew it in its native owner; the previous quota cache was kept."
	}
	return map[string]any{"ok": false, "error": code, "message": message, "cache_preserved": true}
}
func QuotaRefresh(root, account string) (map[string]any, error) {
	return quotaRefresh(root, account, callQuota)
}
func quotaRefresh(root, account string, call quotaCaller) (map[string]any, error) {
	if !validAccount(account, accountProvider(account)) {
		return quotaFailure("invalid_account"), nil
	}
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	all, _, e := readAuthFile(root)
	if e != nil {
		return nil, e
	}
	seedAccounts(root, &r, all)
	entry, reason := quotaAccount(root, r, all, quotaCapabilities(root), account)
	if reason != "" {
		return quotaFailure(reason), nil
	}
	reading, e := call(root, entry, accountProvider(account), account)
	if e != nil {
		if errors.Is(e, errHostDeadline) {
			return quotaFailure("timeout"), nil
		}
		var native nativeQuotaError
		if errors.As(e, &native) {
			return quotaFailure(nativeQuotaCode(string(native))), nil
		}
		return quotaFailure("provider_error"), nil
	}
	unlock, e := Lock(root)
	if e != nil {
		return nil, e
	}
	defer unlock()
	current, e := Read(root)
	if e != nil {
		return nil, e
	}
	currentAuth, _, e := readAuthFile(root)
	if e != nil {
		return nil, e
	}
	seedAccounts(root, &current, currentAuth)
	now, reason := quotaAccount(root, current, currentAuth, quotaCapabilities(root), account)
	if reason != "" || now.Path != entry.Path || now.SHA256 != entry.SHA256 || now.Source != entry.Source {
		return quotaFailure("account_or_plugin_changed"), nil
	}
	if reading.codex && !sameQuotaAuth(all[account], currentAuth[account]) {
		return quotaFailure("account_or_plugin_changed"), nil
	}
	cache, e := readQuotaCache(root)
	if e != nil {
		return nil, e
	}
	if len(cache.Accounts) >= 4096 {
		if _, ok := cache.Accounts[account]; !ok {
			return quotaFailure("cache_limit"), nil
		}
	}
	cache.Accounts[account] = reading
	b, e := json.Marshal(cache)
	if e != nil {
		return nil, e
	}
	if len(b) > 4<<20 {
		return quotaFailure("cache_limit"), nil
	}
	if e = Atomic(filepath.Join(root, quotaCacheFile), append(b, '\n'), 0600); e != nil {
		return nil, e
	}
	result := map[string]any{"ok": true, "account": account, "provider": accountProvider(account), "cached_at": reading.At,
		"windows": len(reading.Windows), "message": "Quota cache updated from the selected account's usage hook."}
	// Notification is an optional consequence of this fresh committed reading.
	// It must never erase the cache, fail the update or run from QuotaList.
	if publishQuotaDesktop(root, account, reading) != nil {
		result["desktop_warning"] = "Quota cache was updated; the optional desktop notification was not published."
	}
	return result, nil
}
