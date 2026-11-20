# Optional Claude Code maintenance

This adapter warms an explicitly selected Claude Code subscription through the native Claude CLI. It shares zcalld's bounded maintenance scheduler and durable intent ledger, but uses separate Claude policy, profile bindings, quota parsing and process ownership. It does not use Codex URLs, reset credits or credential files. Installing the package does not enable maintenance or launch Claude.

The interaction and warmup rules come from Magpie revision [`1e174f8f9f613a920203a4658d259456fcab5d0d`](https://github.com/yetone/magpie/tree/1e174f8f9f613a920203a4658d259456fcab5d0d). The extracted helper has a [file-by-file source record](CLAUDE-TUI-SOURCE.json), with its MIT notice and license included in native and Arch packages. Its additional Go text-width dependency is `golang.org/x/text v0.39.0`; the package includes that module's BSD license. No Rust dependency or GUI framework is added.

## What an action does

A quota inspection runs Claude's print-mode `/usage` command. It does not use an interactive session for this read: the pinned reference documents that some Claude versions make a counted model request during interactive startup. The adapter accepts completed structured readings or explicit `/usage` text, rejects failed or ambiguous responses, and never treats a lone `null` meter as a verified unused window. An explicit `limits: []` means that the server reported no meters; it does not invent a five-hour or weekly allowance.

A warmup sends the fixed text `hi` with the fixed `haiku` model. By default it uses the bundled `zcalld-claude-tui`, which starts Claude in a real PTY on Linux/macOS or system ConPTY on Windows. It translates the interactive transcript to stream JSON and strips print-mode flags. It does not fall back to `-p` when interaction fails. An imported, explicitly enabled `claudePrintMode` preference selects the distinct print path for maintenance; it does not change the gateway provider's interaction contract.

The no-extra preference and configured cap on the account's “Usage credits” apply before a send. A full account-wide allowance applies to every model; a recognized scoped allowance applies to its matching model family. A full Opus-only window does not by itself block a Haiku warmup. An unknown full scope cannot silently authorize extra spending when a restriction is active. An expired window is no longer treated as full. These are checks of the latest completed reading, not a server-side spending reservation: another client or the request itself can change the allowance afterward.

An interactive startup is considered potentially side-effecting even before `hi` is written. The adapter checks the bound identity before launch, again after the helper reports ready, and after completion. A changed identity after launch prevents the message from being written, but the result remains `unknown`; it is not advertised as proof that no request occurred. Unknown sends retain a durable pending intent and are not automatically repeated.

## Bind existing native profiles

An installation that has never used Magpie can bind an existing native profile directly. Use the same binding format below, omitting `login_id` and `user_hash`, with the explicit `claude-provider bind plan|apply` command. This path reads only the binding declaration and native profile identity. It does not require a source snapshot or import credentials. New bindings have no automatic schedule and begin with no-extra protection. Rebinding an already-known identity preserves its existing reviewed cap, schedule and pending ledger; a plan cannot silently change the shared CLI executable or workspace trust of other accounts.

```sh
zcalld plugins claude-provider bind plan \
  --file /absolute/private/claude-bindings.json --json
zcalld plugins claude-provider bind apply \
  --file /absolute/private/claude-bindings.json --expect-plan PLAN_HASH --json
```

The rest of this section describes the separate migration path. A direct native binding is an explicit user choice; it is not a workaround used automatically for a migrated login marked expired.

Plan and apply use a caller-selected private source snapshot and an explicit private binding file. They do not discover credentials, sign in, read OAuth tokens or import the source's `auth` blobs. The source login ID, source identity and current native profile must agree. The plan binds the source paths and file hashes, binding contents, current enabled accounts and profile identity; apply computes the plan again and refuses a changed hash.

The binding format is:

```json
{
  "version": 1,
  "claude_binary": "/absolute/path/to/claude",
  "interactive_helper": "/absolute/path/to/zcalld-claude-tui",
  "trust_workspace": false,
  "accounts": [
    {
      "login_id": "SOURCE_LOGIN_ID",
      "config_dir": "/absolute/private/native-config",
      "profile": "/absolute/private/native-config/.claude.json",
      "ownership": "saved",
      "enabled": true
    }
  ]
}
```

Use canonical absolute paths. The binding file, snapshot and profile must be private to the current user. Saved account directories must also be private. On Unix, an owned current-account directory may have the native CLI's normal `0755` mode, but group/world write permissions are rejected; the adapter does not change its mode. Windows uses the current user's owner/DACL checks. Symlinked components are rejected. An optional `user_hash` can bind the SHA-256 of the source login's exact user string; the account identity also includes its email, organization and account identifiers. Raw profile data and credential contents are not stored in the maintenance policy.

Two native layouts are supported:

| Layout | Config directory | Profile | Child environment |
|---|---|---|---|
| Explicit saved or current config | A private directory | `CONFIG/.claude.json` | Explicit `CLAUDE_CONFIG_DIR` |
| Default current account | `HOME/.claude` | `HOME/.claude.json` | Explicit `HOME`/`USERPROFILE`; no inherited `CLAUDE_CONFIG_DIR` |

A saved directory must already exist and must not still be held by the reference application's native account owner. A source login marked lapsed or paused is rejected even if the current profile still has matching identity fields. Duplicate identity/directory bindings are rejected. A `<config-dir>.run-PID-NONCE.lock` marker uses the reference's directory handover convention while the native process is owned. The adapter never moves a current account into saved storage or overwrites the native login. Workspace trust is an explicit binding choice, not an inherited bypass flag. WSL profile translation is not implemented by this helper.

```sh
zcalld plugins maintenance plan --provider claude \
  --source /absolute/private/snapshot \
  --file /absolute/private/claude-bindings.json --json

zcalld plugins maintenance apply --provider claude \
  --source /absolute/private/snapshot \
  --file /absolute/private/claude-bindings.json \
  --expect-plan PLAN_HASH --json

zcalld plugins maintenance status --provider claude --json
```

Plan returns counts, opaque account keys and a hash. Apply creates or updates the private policy and imported history, preserves live intent state, and leaves a new or changed policy disabled. Applying an identical plan does not silently disable an already reviewed policy. These commands neither start the native CLI nor change an operating-system scheduler.

On Arch, the helper's canonical packaged path is `/usr/lib/zcalld-compat/zcalld-claude-tui`; `/usr/bin/zcalld-claude-tui` is its stable command entry. Portable archives carry the same helper beside `zcalld-pluginctl` in their platform layout. Windows bindings use the absolute `.exe` paths. The static `--capabilities` and `--version` operations do not launch Claude or inspect an account.

## Schedules and explicit warmup

The importer preserves the pinned reference's `claudeWarmup` modes: `off`, `week` and `all` for the five-hour plus weekly windows. It also preserves `claudeWarmAts`, using legacy `claudeWarmAt` only when the list is absent. An explicit empty list remains empty. Clock times use the shared scheduler's local-clock, catch-up and duplicate-prevention rules. `claude-warmup.json` history is matched through the proved source account identity; pending, failed, daily and reset markers are retained rather than interpreted as permission to replay a request.

Claude policy and history live in `extensions/maintenance/claude/state.json`. Codex continues to use `extensions/maintenance/state.json`. No Claude policy may contain Codex weekly-reset or expiring-reset actions. Unknown new accounts are not added to an enabled schedule automatically.

```sh
# Explicitly enable a reviewed Claude schedule; this does not create a timer.
zcalld plugins maintenance enable --provider claude --json

# One bounded due check for Claude alone.
zcalld plugins maintenance run-once --provider claude --json

# Stop automatic Claude maintenance without changing its stored history.
zcalld plugins maintenance disable --provider claude --json

# An explicit action can run while scheduling remains disabled.
zcalld plugins maintenance warmup --provider claude \
  --account OPAQUE_ACCOUNT_KEY --json
zcalld plugins maintenance warmup-all --provider claude --json
```

Manual commands require an explicit `--provider codex` or `--provider claude`. The all-account action uses at most three workers, includes only explicitly enabled source accounts and skips accounts whose applicable general windows are already running. A selected single-account action is deliberate and can warm an already-running window, subject to the same quota, identity and pending-intent checks. Neither command turns on a schedule.

The existing inactive [scheduler templates](CODEX-MAINTENANCE.md) call `maintenance run-once` without a provider and therefore check both independently enabled stores within one original 60-second budget. They do not add a resident maintenance process. Linux packages install the user unit and timer without enabling them; macOS and Windows ship inactive templates. Activating a scheduler is separate from enabling policy and must follow the platform's documented setup.

## Deadlines and failure records

The original deadline covers quota reads, helper startup, the fixed message and cleanup. For interaction, helper work stops eight seconds before that deadline; its bounded PTY shutdown precedes the outer process cancellation boundary. Windows descendants belong to an owned Job before they can execute. Unix cleanup observes the owned process group, while the helper separately confirms its own PTY session group. Signal delivery alone is not treated as an exit acknowledgement. The code does not claim to control arbitrary children that intentionally escape a Unix process group.

No automatic path clears an unknown warmup merely because a process exited or a later quota read looks plausible. An operator must review such an outcome before allowing another send. Claude has no reset-card consume operation or Codex-style idempotency key. This deliberate distinction prevents a transport retry from becoming an extra subscription request.

## Test and integration boundary

The native acceptance entry uses the built helper with a compiled fake Claude executable, private temporary homes and synthetic profiles. It exercises both profile layouts, real PTY/ConPTY input, print-flag removal, EOF, deadlines and owned-child exit. A separate actual-pluginctl suite covers plan/apply, stale plans, cap refusal, explicit warmup and durable unknown outcomes. Fake-child tests cover parent exit before descendant cleanup. No acceptance test downloads Claude, reads a real account or generates a paid request.

The formal release matrix executes these helpers on Linux x86-64/ARM64, Windows x86-64 and macOS x86-64/ARM64. A cross-build is recorded only as a compile check; a platform is not considered executed until its native job passes. Source and Arch packages contain the helper, attribution, licenses and inactive scheduling files. The maintenance adapter does not register a gateway provider route; that separate adapter must be configured and validated on its own contract.
