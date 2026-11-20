package claudemaintenance

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"

	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

// NativeBindingPlan is independent of Magpie. It accepts the same explicit
// native paths but no source login/user selectors, and never starts Claude.
// New accounts have no automatic maintenance and always begin with NoExtra.
func NativeBindingPlan(ctx context.Context, root, file, expected string, apply bool) (PlanSummary, error) {
	result := PlanSummary{Provider: "claude", AccountIDs: []string{}}
	if !filepath.IsAbs(file) {
		return result, ErrConfiguration
	}
	raw, err := readPrivate(file)
	if err != nil {
		return result, err
	}
	var bindings Bindings
	if decode(raw, &bindings) != nil || bindings.Version != 1 || len(bindings.Accounts) == 0 || len(bindings.Accounts) > maintenance.MaxAccounts {
		return result, ErrConfiguration
	}
	binary, err := executable(bindings.Binary)
	if err != nil {
		return result, err
	}
	helper, err := executable(bindings.Helper)
	if err != nil {
		return result, err
	}
	doc, err := Store(root).Read()
	if err != nil {
		return result, err
	}
	previous := maintenance.DocumentDigest(doc)
	c := config{Version: 1, Binary: binary, Helper: helper, TrustWorkspace: bindings.TrustWorkspace, Accounts: map[string]account{}}
	if len(doc.Adapter) > 0 {
		if decode(doc.Adapter, &c) != nil || c.Version != 1 || c.Accounts == nil {
			return result, ErrConfiguration
		}
		// A bind must not silently change every existing account's executable,
		// print preference or workspace trust while adding a different account.
		if c.Binary != binary || c.Helper != helper || c.TrustWorkspace != bindings.TrustWorkspace {
			return result, errors.New("existing Claude runtime differs; review its configuration separately")
		}
	}
	seenIdentity, seenPath := map[string]bool{}, map[string]bool{}
	for _, binding := range bindings.Accounts {
		if binding.LoginID != "" || binding.UserHash != "" || !binding.Enabled {
			return result, ErrConfiguration
		}
		identity, err := profileIdentity(binding.Profile)
		if err != nil {
			return result, err
		}
		id := identity.digest()
		if seenIdentity[id] || seenPath[binding.ConfigDir] {
			return result, ErrConfiguration
		}
		seenIdentity[id] = true
		seenPath[binding.ConfigDir] = true
		key := "builtin:claude#" + digest([]byte("zcalld:claude-native-binding:v1\x00" + id))[:20]
		a := account{ConfigDir: binding.ConfigDir, Profile: binding.Profile, Ownership: binding.Ownership, Identity: id, Enabled: true, Automatic: false, NativeBinding: true, NoExtra: true}
		matches := 0
		for oldKey, old := range c.Accounts {
			if old.Identity == id {
				matches++
				if matches > 1 {
					return result, ErrConfiguration
				}
				key = oldKey
				a.Automatic = old.Automatic
				a.NoExtra = old.NoExtra
				a.ExtraShare = old.ExtraShare
				a.ExtraAmount = old.ExtraAmount
				a.Proxy = old.Proxy
			} else if old.ConfigDir == binding.ConfigDir || old.Profile == binding.Profile {
				return result, ErrConfiguration
			}
		}
		if err = validateAccount(a); err != nil {
			return result, err
		}
		if _, err = accountHome(a); err != nil {
			return result, err
		}
		c.Accounts[key] = a
		result.AccountIDs = append(result.AccountIDs, key)
		if a.Automatic {
			result.AutomaticAccounts++
		}
	}
	if len(c.Accounts) > maintenance.MaxAccounts {
		return result, ErrConfiguration
	}
	sort.Strings(result.AccountIDs)
	result.Accounts = len(result.AccountIDs)
	result.Enabled = doc.Policy.Enabled
	adapter, err := json.Marshal(c)
	if err != nil {
		return result, err
	}
	plan, _ := json.Marshal(struct {
		File, InputDigest, Previous, AdapterDigest string
		Accounts                                   []string
	}{file, digest(raw), previous, digest(adapter), result.AccountIDs})
	result.PlanHash = digest(plan)
	if !apply {
		return result, nil
	}
	if expected == "" || expected != result.PlanHash {
		return result, errors.New("Claude binding plan changed; inspect a fresh plan")
	}
	recheck := func() error {
		fresh, e := readPrivate(file)
		if e != nil || digest(fresh) != digest(raw) {
			return ErrConfiguration
		}
		for _, key := range result.AccountIDs {
			if e = validateAccount(c.Accounts[key]); e != nil {
				return e
			}
		}
		return ctx.Err()
	}
	if err = Store(root).BindAdapter(ctx, previous, result.PlanHash, adapter, result.AccountIDs, recheck); err != nil {
		return result, err
	}
	result.Applied = true
	return result, nil
}
