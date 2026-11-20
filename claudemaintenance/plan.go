package claudemaintenance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

type sourceLogin struct {
	ID      string   `json:"id"`
	Agent   string   `json:"agent"`
	User    string   `json:"user"`
	Profile identity `json:"profile"`
	On      bool     `json:"on"`
	Active  bool     `json:"active"`
	Held    bool     `json:"held"`
	Paused  bool     `json:"paused"`
	Lapsed  string   `json:"lapsed"`
}
type sourceSettings struct {
	Warmup  string   `json:"claudeWarmup"`
	WarmAt  string   `json:"claudeWarmAt"`
	WarmAts []string `json:"claudeWarmAts"`
	NoExtra bool     `json:"claudeNoExtraUsage"`
	Print   bool     `json:"claudePrintMode"`
}
type sourceProvider struct {
	ID             string                        `json:"id"`
	Proxy          string                        `json:"proxy"`
	AccountProxies map[string]string             `json:"accountProxies"`
	Caps           map[string]map[string]int     `json:"accountWindowCaps"`
	Amounts        map[string]map[string]float64 `json:"accountWindowAmounts"`
}
type PlanSummary struct {
	Provider            string   `json:"provider"`
	PlanHash            string   `json:"plan_hash"`
	Accounts            int      `json:"accounts"`
	AccountIDs          []string `json:"account_ids"`
	AutomaticAccounts   int      `json:"automatic_accounts"`
	Applied             bool     `json:"applied"`
	Enabled             bool     `json:"enabled"`
	CredentialsImported bool     `json:"credentials_imported"`
	SchedulerChanged    bool     `json:"scheduler_changed"`
}

// Plan reads only a caller-selected private snapshot and binding declaration.
// OAuth blobs in logins.json are ignored, never copied into the resulting state.
func Plan(ctx context.Context, root, source, bindingFile, expected string, apply bool) (PlanSummary, error) {
	result := PlanSummary{Provider: "claude", AccountIDs: []string{}}
	if !filepath.IsAbs(source) || !filepath.IsAbs(bindingFile) || !noLinks(source) {
		return result, ErrConfiguration
	}
	if !privateDirectory(source) {
		return result, ErrConfiguration
	}
	bindingsBytes, err := readPrivate(bindingFile)
	if err != nil {
		return result, err
	}
	var bindings Bindings
	if decode(bindingsBytes, &bindings) != nil || bindings.Version != 1 || len(bindings.Accounts) > maintenance.MaxAccounts {
		return result, ErrConfiguration
	}
	if bindings.Binary, err = executable(bindings.Binary); err != nil {
		return result, err
	}
	files := map[string][]byte{}
	for _, name := range []string{"settings.json", "logins.json", "providers.json"} {
		files[name], err = readPrivate(filepath.Join(source, name))
		if err != nil {
			return result, err
		}
	}
	files["claude-warmup.json"], err = readPrivate(filepath.Join(source, "claude-warmup.json"))
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(source, "claude-warmup.json")); !errors.Is(statErr, os.ErrNotExist) {
			return result, err
		}
		files["claude-warmup.json"] = nil
	}
	var settings sourceSettings
	var logins []sourceLogin
	var providers struct {
		Providers []sourceProvider `json:"providers"`
	}
	if json.Unmarshal(files["settings.json"], &settings) != nil || json.Unmarshal(files["logins.json"], &logins) != nil || json.Unmarshal(files["providers.json"], &providers) != nil || len(logins) > 4096 {
		return result, ErrConfiguration
	}
	if !settings.Print {
		if bindings.Helper, err = executable(bindings.Helper); err != nil {
			return result, err
		}
	}
	var provider sourceProvider
	seenProvider := false
	for _, p := range providers.Providers {
		if p.ID == "claude" {
			if seenProvider {
				return result, ErrConfiguration
			}
			provider = p
			seenProvider = true
		}
	}
	c := config{Version: 1, Binary: bindings.Binary, Helper: bindings.Helper, Print: settings.Print, TrustWorkspace: bindings.TrustWorkspace, Accounts: map[string]account{}}
	links := []maintenance.AccountLink{}
	seenID := map[string]bool{}
	seenDir := map[string]bool{}
	seenIdentity := map[string]bool{}
	for _, b := range bindings.Accounts {
		if b.LoginID == "" || seenID[b.LoginID] || seenDir[b.ConfigDir] {
			return result, ErrConfiguration
		}
		seenID[b.LoginID] = true
		seenDir[b.ConfigDir] = true
		var login *sourceLogin
		for i := range logins {
			l := &logins[i]
			if l.Agent == "claude" && l.ID == b.LoginID {
				if login != nil {
					return result, ErrConfiguration
				}
				login = l
			}
		}
		if login == nil || !login.Profile.valid() || login.Lapsed != "" || login.Paused || !b.Enabled || (b.UserHash != "" && b.UserHash != digest([]byte(login.User))) {
			return result, ErrConfiguration
		}
		if b.Ownership == "saved" && login.Held {
			return result, errors.New("saved Claude account still has its native owner")
		}
		ident := login.Profile.digest()
		if seenIdentity[ident] {
			return result, errors.New("Claude identity has more than one credential directory")
		}
		seenIdentity[ident] = true
		key := "builtin:claude#" + digest([]byte("zcalld:claude-binding:v1\x00" + login.ID + "\x00" + ident))[:20]
		user := strings.ToLower(strings.TrimSpace(login.User))
		a := account{ConfigDir: b.ConfigDir, Profile: b.Profile, Ownership: b.Ownership, Identity: ident, Enabled: true, Automatic: login.On || login.Held || login.Active, NoExtra: settings.NoExtra, Proxy: provider.Proxy}
		if value, ok := provider.AccountProxies[user]; ok {
			a.Proxy = value
		}
		a.ExtraShare = provider.Caps[user]["usage credits"]
		a.ExtraAmount = provider.Amounts[user]["usage credits"]
		if a.ExtraShare < 0 || a.ExtraShare > 100 || a.ExtraAmount < 0 || math.IsNaN(a.ExtraAmount) || math.IsInf(a.ExtraAmount, 0) || (a.ExtraShare > 0 && a.ExtraAmount > 0) {
			return result, ErrConfiguration
		}
		if a.ExtraShare == 100 {
			a.ExtraShare = 0
		}
		if err = validateAccount(a); err != nil {
			return result, err
		}
		if _, err = accountHome(a); err != nil {
			return result, err
		}
		if _, err = proxyEnvironment(a.Proxy); err != nil {
			return result, err
		}
		c.Accounts[key] = a
		links = append(links, maintenance.AccountLink{SourceUser: login.User, Account: key, Enabled: true})
		result.AccountIDs = append(result.AccountIDs, key)
		if a.Automatic {
			result.AutomaticAccounts++
		}
	}
	// Reuse the tested normalisation/clock/history rules without copying reset
	// policy. Claude's source has no account-specific clock override.
	translated := map[string]any{"codexWarmup": settings.Warmup, "codexWarmAt": settings.WarmAt}
	if settings.WarmAts != nil {
		translated["codexWarmAts"] = settings.WarmAts
	}
	b, _ := json.Marshal(translated)
	warm := files["claude-warmup.json"]
	if warm == nil {
		warm = []byte("{}")
	}
	p, err := maintenance.PlanImport(b, warm, links)
	if err != nil {
		return result, err
	}
	var legacy map[string]map[string]maintenance.WarmWindow
	if json.Unmarshal(warm, &legacy) != nil {
		return result, ErrConfiguration
	}
	for _, link := range links {
		if _, ok := p.Accounts[link.Account]; !ok {
			p.Accounts[link.Account] = maintenance.AccountPolicy{ResetLeadMinutes: 30}
		}
		if !c.Accounts[link.Account].Automatic {
			p.Accounts[link.Account] = maintenance.AccountPolicy{ResetLeadMinutes: 30}
		}
		// Manual actions must also retain uncertain history when scheduling was
		// off in the source. An absent schedule is not permission to replay it.
		if p.Windows[link.Account] == nil {
			p.Windows[link.Account] = legacy[strings.ToLower(strings.TrimSpace(link.SourceUser))]
		}
	}
	p.Adapter, _ = json.Marshal(c)
	bound := map[string]any{"source": source, "bindings_path": bindingFile, "bindings_sha256": digest(bindingsBytes), "files": map[string]any{}}
	for name, data := range files {
		bound["files"].(map[string]any)[name] = map[string]any{"exists": data != nil, "sha256": digest(data)}
	}
	bb, _ := json.Marshal(bound)
	p.SourceDigest = digest(bb)
	maintenance.SealPlan(&p)
	sort.Strings(result.AccountIDs)
	result.Accounts = len(p.Accounts)
	result.PlanHash = p.Hash
	if !apply {
		return result, nil
	}
	if expected == "" || expected != p.Hash {
		return result, errors.New("Claude maintenance plan changed; inspect a fresh plan")
	}
	// Recheck all native profile identities immediately before the durable apply.
	for _, a := range c.Accounts {
		if err = validateAccount(a); err != nil {
			return result, err
		}
	}
	current, err := readPrivate(bindingFile)
	if err != nil || digest(current) != digest(bindingsBytes) {
		return result, errors.New("Claude binding changed during apply")
	}
	for name, previous := range files {
		current, err = readPrivate(filepath.Join(source, name))
		if previous == nil {
			if _, e := os.Lstat(filepath.Join(source, name)); !errors.Is(e, os.ErrNotExist) {
				return result, errors.New("Claude snapshot changed during apply")
			}
		} else if err != nil || digest(current) != digest(previous) {
			return result, errors.New("Claude snapshot changed during apply")
		}
	}
	s := Store(root)
	old, err := s.Read()
	if err != nil {
		return result, err
	}
	enable := old.ImportDigest == p.Hash && old.Policy.Enabled
	if err = s.ApplyPlan(ctx, p, expected, enable, time.Now()); err != nil {
		return result, err
	}
	result.Applied = true
	result.Enabled = enable
	return result, nil
}

func Status(root string) (any, error) {
	s, err := Store(root).Status()
	if err != nil {
		return nil, err
	}
	d, err := Store(root).Read()
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for key := range d.Policy.Accounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return map[string]any{"provider": "claude", "status": s, "account_ids": keys, "local_only": true}, nil
}
