package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/builtin"
	"github.com/LIghtJUNction/zcalld-compat/claudemaintenance"
	"github.com/LIghtJUNction/zcalld-compat/community"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

func runMaintenanceWorker(root, source, expected, inbox string) error {
	if source != "" || expected != "" || inbox != "" {
		return errors.New("maintenance worker takes only --root")
	}
	return maintenance.ServeWorker(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, account string, trigger maintenance.WeeklyTrigger) (maintenance.ActionResult, error) {
		return community.MaintenanceWeekly(ctx, root, account, trigger)
	})
}

func runMaintenance(root, source, expected, inbox, provider, file, account string, args []string) (any, error) {
	if len(args) != 1 {
		return nil, errors.New("maintenance expects status, plan, apply, enable, disable, run-once, warmup, or warmup-all")
	}
	action := args[0]
	if provider != "" && provider != "codex" && provider != "claude" {
		return nil, errors.New("maintenance provider is unsupported")
	}
	if file != "" && (provider != "claude" || (action != "plan" && action != "apply")) {
		return nil, errors.New("maintenance --file is only a Claude plan/apply binding")
	}
	if (action == "warmup" && account == "") || (action != "warmup" && account != "") {
		return nil, errors.New("maintenance --account is required only by warmup")
	}
	if (action == "warmup" || action == "warmup-all") && provider == "" {
		return nil, errors.New("manual warmup requires an explicit provider")
	}
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
	if provider == "claude" {
		store = claudemaintenance.Store(root)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, maintenance.DefaultBudget)
	defer cancel()
	switch action {
	case "status":
		if provider == "claude" {
			return claudemaintenance.Status(root)
		}
		return store.Status()
	case "plan", "apply":
		if provider == "claude" {
			return claudemaintenance.Plan(ctx, root, source, file, expected, action == "apply")
		}
		return builtin.MaintenancePlan(root, source, expected, action == "apply")
	case "enable", "disable":
		if err := store.SetEnabled(ctx, action == "enable", time.Now()); err != nil {
			return nil, err
		}
		status, err := store.Status()
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "enabled": status.Enabled, "accounts": status.Accounts, "scheduler_changed": false, "restart_required": false}, nil
	case "run-once":
		if provider == "claude" {
			return claudemaintenance.Run(ctx, root)
		}
		codex, err := community.MaintenanceRun(ctx, root, inbox)
		if err != nil {
			return nil, err
		}
		if provider == "codex" {
			return codex, nil
		}
		claude, err := claudemaintenance.Run(ctx, root)
		if err != nil {
			return nil, err
		}
		codex.Results = append(codex.Results, claude.Results...)
		if claude.Enabled {
			codex.Enabled = true
			if codex.Status == "disabled" {
				codex.Status = claude.Status
			}
		}
		if claude.Status == "deadline" {
			codex.Status = "deadline"
		}
		return codex, nil
	case "warmup", "warmup-all":
		if provider == "claude" {
			return claudemaintenance.Manual(ctx, root, account, action == "warmup-all")
		}
		return community.MaintenanceManual(ctx, root, account, action == "warmup-all")
	default:
		return nil, errors.New("unknown maintenance action")
	}
}
