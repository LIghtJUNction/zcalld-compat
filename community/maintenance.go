package community

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/desktop"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

// MaintenanceRun starts no Bun process for disabled/not-due state. The single
// total deadline includes the existing credential checkpoint/drain allowance.
func MaintenanceRun(ctx context.Context, root, inbox string) (maintenance.Report, error) {
	store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	var host *Host
	defer func() {
		if host != nil {
			host.Close()
		}
	}()
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
			// Registry plugin sources use the importer entry name. The colon
			// form is reserved for the authentication provider/account key.
			host, err = startHostBudget(root, "builtin-codex", operation, total)
			if err != nil {
				return nil, err
			}
		}
		// The JS operation sees only its work budget. Close and callUntil both
		// reuse total instead of starting a second independent shutdown grace.
		host.drainDeadline = total
		params["deadline_ms"] = operation.UnixMilli()
		var raw json.RawMessage
		if err := host.callUntil(method, params, &raw, operation); err != nil {
			return nil, err
		}
		if len(raw) > 64<<10 {
			return nil, errors.New("maintenance response exceeded its bound")
		}
		return raw, nil
	}
	return (maintenance.Engine{Store: store, Host: maintenance.RPC{Call: call}, Observe: maintenanceObserver(root, inbox)}).Run(ctx)
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
