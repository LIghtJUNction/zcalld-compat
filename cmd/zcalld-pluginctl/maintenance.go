package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/builtin"
	"github.com/LIghtJUNction/zcalld-compat/community"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

func runMaintenance(root, source, expected, inbox string, args []string) (any, error) {
	if len(args) != 1 {
		return nil, errors.New("maintenance expects status, plan, apply, enable, disable, or run-once")
	}
	action := args[0]
	if source != "" && action != "plan" && action != "apply" {
		return nil, errors.New("maintenance --source is only used by plan/apply")
	}
	if expected != "" && action != "apply" {
		return nil, errors.New("maintenance --expect-plan is only used by apply")
	}
	if inbox != "" && (action != "run-once" || !filepath.IsAbs(inbox)) {
		return nil, errors.New("maintenance --desktop-inbox must be an absolute run-once option")
	}
	store := maintenance.Store{Directory: filepath.Join(root, "maintenance")}
	switch action {
	case "status":
		return store.Status()
	case "plan", "apply":
		return builtin.MaintenancePlan(root, source, expected, action == "apply")
	case "enable", "disable":
		if err := store.SetEnabled(context.Background(), action == "enable", time.Now()); err != nil {
			return nil, err
		}
		status, err := store.Status()
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "enabled": status.Enabled, "accounts": status.Accounts, "scheduler_changed": false, "restart_required": false}, nil
	case "run-once":
		return community.MaintenanceRun(context.Background(), root, inbox)
	default:
		return nil, errors.New("unknown maintenance action")
	}
}
