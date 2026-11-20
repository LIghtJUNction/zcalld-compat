package builtin

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LIghtJUNction/zcalld-compat/community"
)

// Policy is separate from credential files and their migration fingerprints.
type codexPolicyPlan struct {
	Hash       string
	Desired    map[string]bool
	Matched    int
	Restricted int
	Enabled    int
	Enable     int
	Disable    int
	Unchanged  int
	Unmapped   int
}
type policyLogin struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	User  string `json:"user"`
}

func policyValue(raw json.RawMessage) (bool, error) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, errors.New("invalid Codex account policy")
	}
	field, ok := value["codex_no_credits"]
	if !ok || len(value) != 1 {
		return false, errors.New("Codex account policy requires an explicit codex_no_credits boolean")
	}
	switch string(bytes.TrimSpace(field)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("Codex account policy requires an explicit codex_no_credits boolean")
	}
}
func buildCodexPolicyPlan(root, source string, registry community.Registry) (codexPolicyPlan, error) {
	p := codexPolicyPlan{Desired: map[string]bool{}}
	var mapping Mapping
	if err := read(filepath.Join(root, "builtin-auth-map.json"), &mapping); err != nil {
		return p, err
	}
	if mapping.Version != 1 || mapping.Source != source {
		return p, errors.New("policy source does not match the private builtin migration map")
	}
	var settings struct {
		NoCredits json.RawMessage `json:"codexNoCredits"`
	}
	if err := read(filepath.Join(source, "settings.json"), &settings); err != nil {
		return p, err
	}
	var users []string
	if len(settings.NoCredits) != 0 && string(bytes.TrimSpace(settings.NoCredits)) != "null" {
		if json.Unmarshal(settings.NoCredits, &users) != nil {
			return p, errors.New("source codexNoCredits must be a list of account users")
		}
	}
	restricted := map[string]bool{}
	for _, user := range users {
		user = strings.ToLower(strings.TrimSpace(user))
		if user == "" || len(user) > 1024 || strings.ContainsAny(user, "\r\n\x00") {
			return p, errors.New("invalid source Codex policy identity")
		}
		restricted[user] = true
	}
	var logins []policyLogin
	loginsPath := filepath.Join(source, "logins.json")
	if err := read(loginsPath, &logins); err != nil {
		return p, err
	}
	matchedUsers := map[string]bool{}
	seenKeys := map[string]bool{}
	identities := map[string]any{}
	for _, account := range mapping.Accounts {
		if account.Agent != "codex" {
			continue
		}
		if (account.Key != "builtin:codex" && !strings.HasPrefix(account.Key, "builtin:codex#")) || seenKeys[account.Key] {
			return p, errors.New("invalid or duplicate Codex migration identity")
		}
		seenKeys[account.Key] = true
		user := ""
		var identity []any
		for _, origin := range account.Sources {
			if origin.Kind != "logins" {
				continue
			}
			if filepath.Clean(origin.Path) != loginsPath || origin.UserHash == "" {
				return p, errors.New("Codex policy login source does not match the migration identity")
			}
			var found *policyLogin
			for i := range logins {
				login := &logins[i]
				if login.Agent != "codex" {
					continue
				}
				if (origin.LoginID != "" && login.ID == origin.LoginID) ||
					(origin.LoginID == "" && hash(login.User) == origin.UserHash) {
					if found != nil {
						return p, errors.New("Codex policy login identity is ambiguous")
					}
					found = login
				}
			}
			if found == nil || hash(found.User) != origin.UserHash {
				return p, errors.New("Codex policy login identity changed or was removed")
			}
			next := strings.ToLower(strings.TrimSpace(found.User))
			if next == "" || (user != "" && user != next) {
				return p, errors.New("Codex policy login identity is ambiguous")
			}
			user = next
			identity = append(identity, object{"login_id": origin.LoginID, "user_hash": origin.UserHash})
		}
		if user == "" {
			continue // Native-only accounts have no proven source-user relationship.
		}
		meta, exists := registry.Accounts[account.Key]
		if !exists {
			continue
		}
		desired := restricted[user]
		p.Desired[account.Key] = desired
		identities[account.Key] = identity
		p.Matched++
		if meta.Enabled {
			p.Enabled++
		}
		if desired {
			p.Restricted++
			matchedUsers[user] = true
		}
		current := false
		if raw, present := registry.AccountPolicies[account.Key]; present {
			var err error
			current, err = policyValue(raw)
			if err != nil {
				return p, err
			}
		}
		switch {
		case desired && !current:
			p.Enable++
		case !desired && current:
			p.Disable++
		default:
			p.Unchanged++
		}
	}
	var normalizedUsers []string
	for user := range restricted {
		normalizedUsers = append(normalizedUsers, user)
		if !matchedUsers[user] {
			p.Unmapped++
		}
	}
	sort.Strings(normalizedUsers)
	// Include identities, source policy and current target policy, never tokens,
	// ImportedHash or SecretHash. A token rotation cannot invalidate this plan.
	p.Hash = digest(object{"source": source, "users": normalizedUsers, "identities": identities,
		"desired": p.Desired, "current": registry.AccountPolicies, "accounts": registry.Accounts})
	return p, nil
}

// CodexPolicy plans or explicitly synchronizes only the no-credits policy.
// It never opens, imports, overwrites, or refreshes target credentials.
func CodexPolicy(root, source, expected string, apply bool) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) {
		return nil, errors.New("policy paths must be absolute")
	}
	var err error
	source, err = canonical(source)
	if err != nil {
		return nil, err
	}
	if apply {
		if len(expected) != 64 {
			return nil, errors.New("policy sync requires --expect-plan from a current policy plan")
		}
		unlock, err := community.Lock(root)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	registry, err := community.Read(root)
	if err != nil {
		return nil, err
	}
	plan, err := buildCodexPolicyPlan(root, source, registry)
	if err != nil {
		return nil, err
	}
	if apply {
		if expected != plan.Hash {
			return nil, errors.New("policy inputs changed; inspect a new plan before synchronizing")
		}
		if plan.Unmapped != 0 {
			return nil, errors.New("source no-credits policy has no unambiguous imported account; no policy changed")
		}
		if registry.AccountPolicies == nil {
			registry.AccountPolicies = map[string]json.RawMessage{}
		}
		for key, restricted := range plan.Desired {
			if restricted {
				registry.AccountPolicies[key] = json.RawMessage(`{"codex_no_credits":true}`)
			} else {
				delete(registry.AccountPolicies, key)
			}
		}
		if err = community.Save(root, registry); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"ok": true, "read_only": !apply, "plan_hash": plan.Hash,
		"matched_accounts": plan.Matched, "enabled_accounts": plan.Enabled,
		"restricted_accounts": plan.Restricted, "enable": plan.Enable,
		"disable": plan.Disable, "unchanged": plan.Unchanged,
		"unmapped_policy_users": plan.Unmapped, "blocked": plan.Unmapped != 0,
		"credentials_changed": false, "restart_required": false,
	}, nil
}
