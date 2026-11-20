package community

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

// MaintenanceManual is an explicit action. It does not enable scheduled work;
// every account still passes the normal verified host and NoCredits guards.
func MaintenanceManual(ctx context.Context, root, key string, all bool) (maintenance.Report, error) {
	registry, err := Read(root)
	if err != nil {
		return maintenance.Report{}, err
	}
	keys := []string{}
	if all {
		for k, meta := range registry.Accounts {
			if meta.Enabled && accountProvider(k) == "builtin:codex" && validAccount(k, "builtin:codex") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
	} else {
		if !validAccount(key, "builtin:codex") || !registry.Accounts[key].Enabled {
			return maintenance.Report{}, errors.New("maintenance account is unavailable")
		}
		keys = []string{key}
	}
	if len(keys) == 0 {
		return maintenance.Report{Status: "complete", Results: []maintenance.AccountResult{}}, nil
	}
	engine := maintenance.Engine{Store: maintenance.Store{Directory: filepath.Join(root, "maintenance")}, ManualHost: func(string) (maintenance.Host, func()) {
		local, close := maintenanceEngine(root, "")
		return local.Host, close
	}}
	return engine.WarmAccounts(ctx, keys, all)
}

// MaintenanceRun starts no Bun process for disabled/not-due state. The single
// total deadline includes the existing credential checkpoint/drain allowance.
func MaintenanceRun(ctx context.Context, root, inbox string) (maintenance.Report, error) {
	engine, close := maintenanceEngine(root, inbox)
	defer close()
	return engine.Run(ctx)
}

// MaintenanceWeekly is called only by the private native request-path worker.
// The state/NoCredits fast paths are local. Authority is rechecked again in the
// verified builtin host immediately before any eventual remote action.
func MaintenanceWeekly(ctx context.Context, root, account string, trigger maintenance.WeeklyTrigger) (maintenance.ActionResult, error) {
	store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	state, err := store.Read()
	if err != nil {
		return maintenance.ActionResult{Status: "not_sent", Code: "state_unavailable"}, err
	}
	if !state.Policy.Enabled || !state.Policy.Accounts[account].WeeklyReset {
		return maintenance.ActionResult{Status: "not_sent", Code: "weekly_disabled"}, nil
	}
	registry, err := Read(root)
	if err != nil {
		return maintenance.ActionResult{Status: "not_sent", Code: "account_unavailable"}, err
	}
	meta, ok := registry.Accounts[account]
	if !ok || !meta.Enabled {
		return maintenance.ActionResult{Status: "not_sent", Code: "account_unavailable"}, nil
	}
	if trigger == maintenance.WeeklyBeforeCredits {
		if raw, ok := registry.AccountPolicies[account]; ok {
			var policy struct {
				NoCredits bool `json:"codex_no_credits"`
			}
			if json.Unmarshal(raw, &policy) != nil {
				return maintenance.ActionResult{Status: "not_sent", Code: "policy_unavailable"}, errors.New("invalid current account policy")
			}
			if policy.NoCredits {
				return maintenance.ActionResult{Status: "not_sent", Code: "no_credits"}, nil
			}
		}
	}
	engine, close := maintenanceEngine(root, "")
	defer close()
	return engine.RunWeekly(ctx, account, trigger)
}

func maintenanceEngine(root, inbox string) (maintenance.Engine, func()) {
	store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	var host *Host
	close := func() {
		if host != nil {
			host.Close()
		}
	}
	call := func(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
		if method != "maintenance.inspect" && method != "maintenance.warmup" && method != "maintenance.consume" {
			return nil, errors.New("invalid maintenance operation")
		}
		total, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("maintenance requires a total deadline")
		}
		operation := total.Add(-35 * time.Second)
		if time.Now().After(operation) || ctx.Err() != nil {
			return nil, errHostDeadline
		}
		if host == nil {
			var err error
			host, err = startBuiltinCodexHostContext(ctx, root, operation, total)
			if err != nil {
				return nil, err
			}
		}
		// The JS operation sees only its work budget. Close and callUntil both
		// reuse total instead of starting a second independent shutdown grace.
		host.drainDeadline = total
		params["deadline_ms"] = operation.UnixMilli()
		var raw json.RawMessage
		if err := host.callUntilContext(ctx, method, params, &raw, operation); err != nil {
			// Keep Engine's private ledger lock until the owned host finishes
			// its credential drain. A cancelled caller cannot overlap the
			// next reset with an old host's final credential checkpoint.
			host.Close()
			return nil, err
		}
		if len(raw) > 64<<10 {
			return nil, errors.New("maintenance response exceeded its bound")
		}
		return raw, nil
	}
	return maintenance.Engine{Store: store, Host: maintenance.RPC{Call: call}, Observe: maintenanceObserver(root, inbox)}, close
}

// Re-read the host-owned settings for each already-needed observation. A CLI
// inbox argument is only a matching assertion; it cannot enable notifications,
// bypass the configured thresholds, or redirect a private event elsewhere.
func maintenanceObserver(root, requestedInbox string) func(maintenance.Snapshot) error {
	return func(s maintenance.Snapshot) error {
		cfg, err := desktop.ReadConfig(root)
		if err != nil {
			return err
		}
		if !cfg.QuotaEnabled() {
			return nil
		}
		if requestedInbox != "" && requestedInbox != cfg.Inbox {
			return desktop.ErrConfig
		}
		return maintenance.DesktopObserver(cfg.Inbox)(s)
	}
}
