package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/community"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

func maintenanceBytes(path string, maximum int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maximum {
		return nil, errors.New("maintenance snapshot must contain bounded regular files")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) {
		return nil, errors.New("maintenance snapshot changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maximum {
		return nil, errors.New("maintenance snapshot exceeds its size bound")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || int64(len(b)) != before.Size() {
		return nil, errors.New("maintenance snapshot changed while reading")
	}
	return b, nil
}
func byteDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

type maintenancePlan struct {
	Plan    maintenance.ImportPlan
	Hash    string
	Matched int
}

func buildMaintenancePlan(root, source string, registry community.Registry) (maintenancePlan, error) {
	result := maintenancePlan{}
	mapBytes, err := maintenanceBytes(filepath.Join(root, "builtin-auth-map.json"), maxFile)
	if err != nil {
		return result, err
	}
	var mapping Mapping
	if json.Unmarshal(mapBytes, &mapping) != nil || mapping.Version != 1 || !filepath.IsAbs(mapping.Source) {
		return result, errors.New("maintenance requires an existing private builtin identity map")
	}
	files := map[string][]byte{}
	for _, name := range []string{"settings.json", "logins.json", "codex-warmup.json"} {
		b, readErr := maintenanceBytes(filepath.Join(source, name), 2<<20)
		if readErr != nil {
			return result, readErr
		}
		files[name] = b
	}
	var logins []policyLogin
	if json.Unmarshal(files["logins.json"], &logins) != nil || len(logins) > 4096 {
		return result, errors.New("invalid maintenance snapshot identities")
	}
	links := []maintenance.AccountLink{}
	seen := map[string]bool{}
	originalLoginPath := filepath.Join(mapping.Source, "logins.json")
	for _, account := range mapping.Accounts {
		if account.Agent != "codex" {
			continue
		}
		if seen[account.Key] {
			return result, errors.New("duplicate imported Codex identity")
		}
		seen[account.Key] = true
		meta, present := registry.Accounts[account.Key]
		if !present || !meta.Enabled {
			continue
		}
		user := ""
		for _, origin := range account.Sources {
			if origin.Kind != "logins" {
				continue
			}
			if filepath.Clean(origin.Path) != originalLoginPath || origin.UserHash == "" {
				return result, errors.New("maintenance source identity differs from its import map")
			}
			var found *policyLogin
			for i := range logins {
				login := &logins[i]
				if login.Agent != "codex" {
					continue
				}
				if (origin.LoginID != "" && login.ID == origin.LoginID) || (origin.LoginID == "" && hash(login.User) == origin.UserHash) {
					if found != nil {
						return result, errors.New("maintenance snapshot identity is ambiguous")
					}
					found = login
				}
			}
			if found == nil || hash(found.User) != origin.UserHash {
				return result, errors.New("maintenance snapshot identity changed")
			}
			next := strings.ToLower(strings.TrimSpace(found.User))
			if next == "" || (user != "" && next != user) {
				return result, errors.New("maintenance source user is ambiguous")
			}
			user = next
		}
		if user != "" {
			links = append(links, maintenance.AccountLink{SourceUser: user, Account: account.Key, Enabled: true})
		}
	}
	result.Plan, err = maintenance.PlanImport(files["settings.json"], files["codex-warmup.json"], links)
	if err != nil {
		return result, err
	}
	inputs := map[string]any{}
	for name, b := range files {
		inputs[name] = map[string]string{"path": filepath.Join(source, name), "sha256": byteDigest(b)}
	}
	result.Hash = digest(object{"snapshot": source, "files": inputs, "mapping_sha256": byteDigest(mapBytes), "accounts": registry.Accounts, "plan": result.Plan.Hash})
	result.Matched = len(links)
	return result, nil
}

// MaintenancePlan reads an explicitly selected snapshot. It proves its login
// identities against the existing import map; it never opens target auth,
// imports accounts, refreshes tokens, or changes the independent NoCredits rule.
func MaintenancePlan(root, source, expected string, apply bool) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) {
		return nil, errors.New("maintenance snapshot paths must be absolute")
	}
	var err error
	source, err = canonical(source)
	if err != nil {
		return nil, err
	}
	enabled := false
	if apply {
		if len(expected) != 64 {
			return nil, errors.New("maintenance apply requires --expect-plan from a current plan")
		}
		unlock, lockErr := community.Lock(root)
		if lockErr != nil {
			return nil, lockErr
		}
		defer unlock()
	}
	registry, err := community.Read(root)
	if err != nil {
		return nil, err
	}
	p, err := buildMaintenancePlan(root, source, registry)
	if err != nil {
		return nil, err
	}
	blocked := p.Plan.UnmappedAccounts != 0 || len(p.Plan.Accounts) == 0
	if apply {
		if expected != p.Hash {
			return nil, errors.New("maintenance snapshot or account mapping changed; inspect a fresh plan")
		}
		if blocked {
			return nil, errors.New("maintenance has unmapped or no enabled policy accounts")
		}
		store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
		current, readErr := store.Read()
		if readErr != nil {
			return nil, readErr
		}
		// Reapplying the same import must not disable an explicitly enabled
		// policy or reset its live ledger/startup clock.
		enabled = current.ImportDigest == p.Plan.Hash && current.Policy.Enabled
		if err = store.ApplyPlan(context.Background(), p.Plan, p.Plan.Hash, enabled, time.Now()); err != nil {
			return nil, err
		}
	}
	weekly, resets := 0, 0
	for _, p := range p.Plan.Accounts {
		if p.WeeklyWarmup {
			weekly++
		}
		if p.ExpiringReset {
			resets++
		}
	}
	return map[string]any{"ok": true, "read_only": !apply, "plan_hash": p.Hash, "matched_accounts": p.Matched, "policy_accounts": len(p.Plan.Accounts), "weekly_warmup": weekly, "expiring_reset": resets, "unmapped_accounts": p.Plan.UnmappedAccounts, "blocked": blocked, "enabled": enabled, "credentials_changed": false, "no_credits_changed": false, "scheduler_changed": false}, nil
}
