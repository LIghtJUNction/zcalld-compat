# Optional Codex maintenance

The maintenance command imports explicitly selected policies for already mapped Codex accounts. Scheduled warmup and expiring-card checks run once and exit. Weekly reset is a separate request-path action: a timer never spends a weekly reset credit. Installing the package enables neither the policy nor a scheduler.

| Source setting | Imported behavior |
| --- | --- |
| `codexWarmup: off` | No reset-triggered warmup. Explicit clock times remain independent. |
| `codexWarmup: week` | Warm general quota windows of at least 24 hours after reset. |
| `codexWarmup: all` | Also warm shorter general windows, including the five-hour window. |
| `codexWarmAts` / legacy `codexWarmAt` | Local wall-clock starts; normalize and deduplicate times. An explicit empty list disables the old single-clock value. |
| `codexWarmAtOf` | An account clock replaces the global list; `off` disables that account's clock starts. |
| `codexAutoResetOnly: week` | Request-triggered weekly reset only. |
| `codexAutoResetOnly: expiring` | Expiring-card maintenance only. |
| Account enabled in `codexAutoReset`, no `Only` value | Both reset policies; their ledgers remain separate. |

The policy parser is in `maintenance/import.go`. These meanings follow the pinned reference's [settings](https://github.com/yetone/magpie/blob/1e174f8f9f613a920203a4658d259456fcab5d0d/internal/settings/settings.go), [warmup scheduling](https://github.com/yetone/magpie/blob/1e174f8f9f613a920203a4658d259456fcab5d0d/internal/provider/warmup_daily.go), and [request-triggered reset](https://github.com/yetone/magpie/blob/1e174f8f9f613a920203a4658d259456fcab5d0d/internal/gateway/codex_autoreset.go). Invalid modes or clocks are rejected during planning.

## Review and import a snapshot

Use an existing private migration snapshot containing `settings.json`, `logins.json`, and `codex-warmup.json`. The current extension registry and `builtin-auth-map.json` must already contain the accounts. The planner matches each login ID and the hash of its original user field against that import map. It does not import credentials, create accounts, or open the target authentication store.

```sh
zcalld plugins maintenance status
zcalld plugins maintenance plan --source /absolute/private/snapshot
zcalld plugins maintenance apply --source /absolute/private/snapshot --expect-plan PLAN_HASH
```

If present, include `codex-autoreset.json` and `codex-resets-used.json` from the same snapshot. The first records the original week's spent-reset guard; the second is its recovery audit. Missing files mean no imported spend. An unreadable or malformed primary cannot silently erase that guard: a valid audit supplies a conservative seven-day guard, and unknown history blocks weekly spending for seven days.

Replace `PLAN_HASH` with the hash from the immediately preceding plan. The hash binds the canonical snapshot path, the required and optional file hashes and presence, the existing account map, current registry account states, and the derived policy. Apply recomputes it while holding the registry lock. A changed input requires a new plan. Output contains aggregate counts and the plan hash; it does not print login contents. A first or changed import remains disabled. Reapplying the same import preserves the live ledger and an explicitly enabled policy. A reviewed policy change retains runtime attempts, daily markers, and the more conservative spent-week guard.

The source warmup ledger is retained byte for byte inside the private state file. Its quota labels are reconciled with current official windows only when their duration identifies one unambiguous window. An old pending attempt is not treated as permission to generate again.

The independent NoCredits policy is not part of this import. Sync any newer restrictions separately from the correct source using the existing `codex policy plan|sync` commands. A restriction added after migration must not be described as a missing field in the original snapshot.

## Enable and run

```sh
zcalld plugins maintenance enable
zcalld plugins maintenance run-once
zcalld plugins maintenance status
zcalld plugins maintenance disable
```

`enable` changes only the local policy flag. It does not install a timer, start the gateway, or run maintenance immediately. The engine waits at least two minutes before its first warmup inspection and three minutes before reset inspection. A disabled or not-yet-due invocation does not start Bun. An enabled invocation has one 60-second total budget, including credential cancellation and drain; only the earlier work portion can start requests. A private cross-process lock prevents overlapping runs.

Warmup uses an explicitly imported, currently enabled Codex account. The request uses the normal built-in loader, credential owner checks, and the current NoCredits guard. The prompt and official endpoint are fixed by the adapter. The model must be advertised by the account's model catalog. Unknown quota, a missing catalog, or a NoCredits hold prevents generation. Warmup can spend quota; no live warmup is part of the automated test suite.

A clock start runs at most once for its local date and time. A start missed by no more than one hour can run when the machine resumes; older starts are skipped. A short-window reset warmup is held if it would run across the next requested clock. The corresponding weekly warmup is also held because the same generation would activate both windows. Old whole-date ledger entries still suppress that day's clock starts. A completed warmup whose window remains idle uses the original 15-minute to four-hour backoff.

Reset-credit use is a separate explicit account policy. A fresh inspection must identify the same eligible card twice, including its exact expiry when one exists. Before sending, the engine persists a random `redeem_request_id` and card identity. A retry reuses that identifier and card. It cannot switch to another card after an uncertain result. Immediately before POST, the adapter re-reads the current credential owner, native source file, account, and access-token identity. Any change yields `not_sent`; it does not spend with a stale authorization snapshot. The adapter does not accept caller-supplied URLs, prompts, models, or policy overrides.

### Weekly request-path trigger

Only the native gateway's trusted request path may launch `maintenance worker`. `before_credits` requires NoCredits to be off, a fresh full weekly window with a known future reset, and the reference's `CodexGoesOnCredits` condition: explicit unlimited credits, or `has_credits` with a positive or unparseable balance. Missing usage is not evidence. NoCredits-enabled accounts continue through alternatives first. `routes_exhausted` is allowed only after the native router has exhausted those alternatives; the worker still revalidates the account policy, full window, original week, and card.

The worker uses the same private process lock and durable reset ledger as the timer. It starts no Bun host when the local policy is off, the original week is already recorded, or a NoCredits account reaches the before-credits fast path. Only `completed` with code `reset` tells routing to retry. A duplicate-redemption acknowledgement or unknown outcome does not create a new reset or authorize that retry. The timer never invokes either weekly trigger.

Its private stdin protocol accepts one bounded `weekly` request, followed only by cancellation. Native `abort` or EOF cancels the same context used by the verified builtin Host. Abort, stdin close, and credential drain share one deadline; they do not grant a fresh operation budget. This protocol is not an HTTP management route and has no caller-controlled endpoint or prompt.

## Failure and restart behavior

Policy and execution ledger share one private, atomically replaced `maintenance/state.json`. Before delegating a request to the host, the engine commits its intent and then its sending state. A process loss after the sending transition is deliberately treated as uncertain, even if the network request may not have left the machine.

| Result | Subsequent behavior |
| --- | --- |
| Warmup proven not sent | The normal due check may try later. |
| Warmup completed | The ledger records completion and avoids immediate repetition. |
| Warmup outcome unknown | No blind replay. A fresh active window can reconcile the pending state without attributing which request activated it. |
| Reset outcome unknown | Revisit only the same card and durable request ID. A vanished or changed card remains pending for review. |
| Reset reports `nothing_to_reset` | Record that no spend was needed; retain the card for a later independently due action. |
| Deadline or account no longer eligible | Exit with a bounded report and preserve the ledger. |

Do not remove a pending ledger entry merely to make an action retry. Its purpose is to prevent a crash or transport error from becoming an additional request. `status` reads local aggregate state and does not contact an account.

## Optional schedulers

Arch packages and Linux native archives install the service and timer under `/usr/lib/systemd/user/`. All native archives and the Arch package also carry this guide under `share/doc/zcalld-compat/docs/`, plus the macOS and Windows templates under `share/doc/zcalld-compat/packaging/`. Unix archives place that shared directory beneath `usr/`; the Windows ZIP places it at the archive root. None of these schedulers is enabled by package installation. Scheduler activation is an independent deployment action after a reviewed plan has been applied and the policy explicitly enabled.

### Linux

Install the supplied user units, then explicitly enable the timer if desired:

```sh
systemctl --user daemon-reload
systemctl --user enable --now zcalld-maintenance.timer
systemctl --user status zcalld-maintenance.timer
```

The timer runs an ordinary oneshot at approximately one-minute intervals. The policy ledger determines whether any account inspection is due; a timer tick does not imply a network request. The service has a 75-second outer timeout and terminates its owned process group. For another configuration, use a unit drop-in with an explicit `--config /absolute/path` argument. No service restart is required for a policy change.

To stop scheduling as well as disable policy:

```sh
zcalld plugins maintenance disable
systemctl --user disable --now zcalld-maintenance.timer
```

### macOS and Windows

`packaging/launchd/org.zcalld.maintenance.plist.in` is a user LaunchAgent template. Replace `@ZCALLD_BIN@` with the installed absolute executable path and validate the rendered plist before explicitly loading it. It uses a 60-second interval and the same bounded command.

`packaging/windows/zcalld-maintenance.xml.in` is a Task Scheduler template. Render the installed executable path, current user SID, and an explicit start boundary. The task is disabled by default, uses the interactive user's least-privilege token, ignores overlapping starts, and has a 75-second outer limit. Enable it only after reviewing the imported policy.

The templates do not modify system-wide startup settings. Linux unit syntax and template structure are checked locally; macOS and Windows scheduler activation requires validation on the corresponding operating system and is not implied by cross-compiling the helper.

## Optional desktop observations

Maintenance can publish a fresh usage observation that it already obtained for its own due check. Notification delivery never causes an additional account request, and a full or unavailable inbox does not change the maintenance result. Only measured quota windows are included. Currency is not inferred from a credit balance. Reset-credit expiry is included only when the usage count and available-card list agree and a valid earliest expiry is known.

Each observation re-reads the host-owned `desktop.json` next to the extensions directory. Notifications require `enabled` and a nonzero quota alert threshold; missing, disabled, or invalid settings produce no event. The optional `--desktop-inbox` assertion must match that configured inbox and cannot enable or redirect notifications.

Events use an opaque account-derived digest. They contain no account names, access tokens, or reset-card identifiers. The inbox is private, has bounded file and byte limits, and uses the same publication lock as the desktop consumer's claim operation. Completed-event deletion can occur without that lock because it only reduces occupied capacity.

## Validation boundary

The tests use temporary homes, synthetic import snapshots, mock upstream responses, and controlled clocks. They cover changed-plan rejection, duplicate-run locking, imported pending state, crash recovery, same-ID consume retries, no-spend results, clock catchup, weekly versus timer authority, native abort/EOF, bounded host drain, NoCredits routing, and local-only disabled/status paths. They do not exercise a live generation, refresh another application's credentials, or consume a real reset credit. A successful test establishes the tested state transition; it does not prove an external service will accept a future account request.

This module provides Codex maintenance. The pinned reference's Claude warmup uses a different send path and extra-usage guard; those require a separately verified provider hook. It is not implemented by substituting Claude accounts into the Codex URLs or reset-card schema.
