# Optional Codex maintenance

The maintenance command restores two explicitly selected, existing account policies: weekly-window warmup and reset credits approaching expiry. It runs once and exits. Installing the package does not enable either the policy or a scheduler.

This implementation covers the migration shape validated for this project: weekly warmup, no five-hour warmup or daily clock times, and expiring reset credits without weekly reset. An unsupported source policy is rejected during planning. It is not silently widened or approximated. The policy parser is in `maintenance/import.go`.

## Review and import a snapshot

Use an existing private migration snapshot containing `settings.json`, `logins.json`, and `codex-warmup.json`. The current extension registry and `builtin-auth-map.json` must already contain the accounts. The planner matches each login ID and the hash of its original user field against that import map. It does not import credentials, create accounts, or open the target authentication store.

```sh
zcalld plugins maintenance status
zcalld plugins maintenance plan --source /absolute/private/snapshot
zcalld plugins maintenance apply --source /absolute/private/snapshot --expect-plan PLAN_HASH
```

Replace `PLAN_HASH` with the hash from the immediately preceding plan. The hash binds the canonical snapshot path, all three file hashes, the existing account map, current registry account states, and the derived policy. Apply recomputes it while holding the registry lock. A changed input requires a new plan. Output contains aggregate counts and the plan hash; it does not print login contents. A first import remains disabled. Reapplying the same import preserves the live ledger and an explicitly enabled policy.

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

Warmup uses an explicitly imported, currently enabled Codex account. The request uses the normal built-in loader, credential owner checks, and the current NoCredits guard. The prompt and official endpoint are fixed by the adapter. The model must be advertised by the account's model catalog. An unknown quota, missing catalog, held window, or policy rejection prevents generation. Warmup can spend quota; no live warmup is part of the automated test suite.

Reset-credit use is a separate explicit account policy. A fresh inspection must identify the same eligible card twice, including its exact expiry. Before sending, the engine persists a random `redeem_request_id` and card identity. A retry reuses that identifier and card. It cannot switch to another card after an uncertain result. The adapter does not accept caller-supplied URLs, prompts, models, or policy overrides.

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

The tests use temporary homes, synthetic import snapshots, mock upstream responses, and controlled clocks. They cover changed-plan rejection, duplicate-run locking, imported pending state, crash recovery, same-ID consume retries, no-spend results, bounded host deadlines, NoCredits routing, and local-only disabled/status paths. They do not exercise a live generation, refresh another application's credentials, or consume a real reset credit. A successful test establishes the tested state transition; it does not prove an external service will accept a future account request.
