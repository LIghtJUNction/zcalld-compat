# zcalld compatibility component

This optional component preserves the interfaces used by Magpie community plugins and the local privacy engine. It starts no Magpie gateway, opens no network listener of its own, and contains no GUI. The native zcalld core does not link the Go runtime or Bun.

The imported code is fixed at Magpie commit [1e174f8f9f613a920203a4658d259456fcab5d0d](https://github.com/yetone/magpie/tree/1e174f8f9f613a920203a4658d259456fcab5d0d). [UPSTREAM.json](UPSTREAM.json), [NOTICE](NOTICE), and [LICENSE](LICENSE) record its provenance and modifications.

## Components

| Component | Process and purpose |
| --- | --- |
| `zcalld-privacy` | Private stdio worker for masking, local AstrLink Guard inference, and reversible JSON/SSE restoration. One worker is shared by the enabled native privacy plugin. |
| `zcalld-pluginctl` | Short-lived CLI helper for installation, registry changes, private migration, provider metadata, and public plugin RPC. |
| `community/vendor/host.js` | Pinned upstream OpenCode v1 and pi host. The native community plugin starts one Bun process lazily and uses its byte-credit and cancellation protocol. |
| `internal/redact` | Original rule engine, stable keyed placeholders, JSON field selection, known-value masking, and split-event restoration. |
| `internal/pii/guard` | Original AstrLink Guard tokenizer and CPU implementation. Model weights are not included. |

Native libraries and JavaScript packages are trusted code with the user's privileges. A revision or checksum identifies code; it does not sandbox it. New installations start disabled. Import explicitly preserves the enabled state of the user's existing plugins.

## Build

The compatibility worker is built separately from the Rust gateway.

```sh
go build -o bin/zcalld-privacy ./cmd/zcalld-privacy
go build -o bin/zcalld-pluginctl ./cmd/zcalld-pluginctl
go test ./privacy ./community ./internal/redact ./internal/lazyre ./internal/pii/guard
```

The parent zcalld workspace builds the native `redact` and `community` adapters through zplugin. Arch packages place helpers in `/usr/lib/zcalld-compat` with CLI links in `/usr/bin`; portable archives use `/usr/libexec/zcalld`. Native plugins are supplied by the core in `/usr/lib/zcalld/plugins`. Bun is needed only for JavaScript plugins; Git is needed only for Git installation sources.

## Manage packages

Every command takes `--root /absolute/config-directory/extensions`. The parent CLI supplies that argument.

```sh
zcalld-pluginctl --root "$PWD/example/extensions" list --json
zcalld-pluginctl --root "$PWD/example/extensions" install /absolute/plugin.mjs --name sample --json
zcalld-pluginctl --root "$PWD/example/extensions" install npm:package-name@1.2.3 --json
zcalld-pluginctl --root "$PWD/example/extensions" install github:owner/repo --revision FULL_COMMIT_HASH --json
zcalld-pluginctl --root "$PWD/example/extensions" enable sample --json
zcalld-pluginctl --root "$PWD/example/extensions" config sample --file /absolute/options.json --json
zcalld-pluginctl --root "$PWD/example/extensions" disable sample --json
zcalld-pluginctl --root "$PWD/example/extensions" remove sample --json
```

Git sources require a full immutable commit. npm sources require an exact version. Local sources are copied and hashed. A supplied `--sha256` must match; native libraries are checked again before activation and loading. Bun package lifecycle scripts are disabled. Local source trees reject symlinks and special files. Installed generations remain separate so an update does not overwrite code serving an existing stream.

Public results include names, paths, kinds, versions, revisions, checksums, and enabled state. They omit options, authentication, and private settings. Configuration files and copied authentication use private permissions and atomic replacement.

## Preserve a Magpie setup

Import into a fresh candidate directory before changing a running service:

```sh
zcalld-pluginctl --root /absolute/candidate/extensions import-magpie \
  --source /absolute/magpie-config \
  --privacy-root /absolute/candidate/privacy \
  --model-root /absolute/magpie-cache --json
```

The importer copies installed npm packages and dependencies, package options, plugin authentication, the provider cache, privacy settings, the existing `redact.key`, masking history, and installed model files. It never starts plugins or refreshes a token during import. It reports counts and generated configuration paths. Existing destination state is rejected. Local/Git plugin specifications that cannot be resolved from the installed npm tree require explicit installation; failure is reported.

The built-in account adapters are separate from the imported community authentication. The import result states `builtin_oauth_imported: false`; the parent migration command handles those accounts.

The generated paths are absolute. Moving a candidate requires rewriting them. Keep the original application active until the parent migration checks and cutover complete; copied rotating credentials should not be refreshed by two gateways concurrently.

## Privacy behavior

The worker reads the settings file for every request. It preserves the Magpie `redact*` flags, custom words and rules, kind actions, allowlist, threshold map, and model selection. `zcalldEnabled: false` disables masking without deleting those settings. Absence of that override honors the imported flags.

The importer retains existing masking history. The worker does not write original values or request bodies to disk; its reversible value map lives in memory. A durable private key keeps placeholders stable. Replies are restored after the gateway converts protocols, including placeholder fragments split between SSE events and tool arguments. Block actions stop egress. An explicitly selected but unavailable model holds requests back; automatic model selection may use rules while model files are absent, matching the upstream request-time fallback. This adapter does not download missing model weights automatically.

The local model adapter currently runs AstrLink Guard. It was tested against the installed pinned 7216b6a3b897 model, including tokenizer and classifier fixtures. Privacy Filter selections are reported as unavailable and fail closed; this component never silently substitutes a different model for an explicit selection.

### Detection history

The worker now appends detection events to `history_dir/YYYY-MM-DD.jsonl`. An
omitted `history_dir` resolves to `history` beside the settings file, so an older
imported binding continues to use its migrated history. New imports write the
explicit absolute path. The day uses local time; each record also includes its
RFC 3339 timestamp.

The format follows [Magpie maskrec at the pinned revision](https://github.com/yetone/magpie/blob/1e174f8f9f613a920203a4658d259456fcab5d0d/internal/maskrec/maskrec.go):
`key`, `t`, `what`, optional `agent` and `session`, `n`, `values`, and
optional `blocked`. Each value has `p` (placeholder), `k` (kind), `b` (finder),
`l` (character length), `n` (occurrences), optional `h` (the upstream first
character or key-prefix hint), and optional `a` (warn/block action).
Repeated placeholders are merged. Original complete values, request bodies,
surrounding text, response content, and restoration mappings are never stored.
Hints intentionally reveal the same small prefix as upstream; the history is
private data.

An event records detection during `Mask`. It does **not** assert that a provider
received the request or completed a generation. This worker cannot reliably
know the final provider, model, trace route, or generation outcome, so those
fields remain absent. Response restoration does not create another event.
A no-hit request creates no JSONL event. Warn and block findings do create one.

Daily `checked-YYYY-MM-DD.json` files retain the upstream
`agent -> {looked, masked, whole}` shape. Every enabled request with actual
content increments `looked`; any finding also increments `masked`. These are
request counts, not value counts. Imported `whole` counts are preserved;
this adapter has no per-model unmasked exception that would increment them.
An empty activation probe and `zcalldEnabled: false` increment nothing.
Counters merge with existing files under a process lock, normally write within
five seconds, and flush on normal stdin EOF.

| Worker operation or field | Contract |
| --- | --- |
| `mask.context` | Optional or null; string `agent` <=512 bytes, `session` <=1024, `what` <=2048. Supplied by the gateway's existing whitelist. |
| `mask.result.audit_error` | Optional fixed string `privacy audit history is unavailable`; no paths or request values. The masked result remains valid. |
| `flush` | Returns `result: true` after pending counters are persisted, or the fixed audit error. It leaves the engine usable. |
| stdin EOF | Stops the timer and flushes remaining counters; failure gives a nonzero exit status. |

Each event retains at most 4,096 distinct values and encodes to at most 1 MiB.
`omitted_hits` explicitly counts finding metadata omitted by those bounds;
the total masking count and the protected request are unchanged. Each day's
counter map keeps at most 1,024 agent identities plus `other`; further
identities add to `other`. Counter files are limited to 1 MiB. Old history is
pruned by whole day: 30 days, and a 100 MiB JSONL retention target checked
hourly while events are written. Checked files have their own 30-day retention.
No daily file is partially truncated.

Directories and files receive private permissions, including a protected DACL
on Windows. The stable lock is a sibling of the history directory:
`privacy/.history.zcalld-audit.lock` for `privacy/history`. It coordinates
multiple workers and cutover without moving with a history-directory exchange.
The lock contains only an integer key allocator and is not history payload.
A new worker reconstructs the maximum key from bounded retained metadata when
needed; history copying does not require copying the lock.

JSONL appends close the file immediately, following upstream's best-effort
audit behavior; they do not claim power-loss durability. Counter snapshots
use a synced temporary file and atomic replacement. Audit storage failure
never disables masking or reveals original values. The RPC error field and
explicit flush acknowledgement let the native adapter report a static
diagnostic even when worker stderr is not attached to a journal. An abruptly
killed worker can lose counter increments still within the five-second window.

## Public plugin operations

`invoke PROVIDER EVENT --input-file /absolute/request.json` calls an allowlisted public host method. Raw authentication exporters and loader results are unavailable through this command. Public results remove credential fields.

OAuth callbacks must run in the same host as authorization. `session` keeps that host alive and accepts JSON lines with `id`, `method`, and `params` on stdin, returning a corresponding sanitized result. It supports authorization, callback, prompts, validation, usage, check-in, account checks, tools, and search. End stdin to stop that helper and its host.

## Validation scope

The privacy wrapper tests cover block behavior, setting reloads, the reversible disable switch, explicit-model failure, actual local-model classification, JSON tool arguments, and split SSE placeholders. Manager tests cover lifecycle changes, immutable Git pins, checksum tampering, copy boundaries, and suppressed package scripts. Parent gateway tests exercise real native libraries and Bun against loopback mock providers.

The model evaluation now replays both checked-in upstream prediction corpora in the default test suite, using the pinned model thresholds without a downloader, network, or user cache. The original full-runtime recapture driver is retained as the reference-only text fixture `internal/redact/testdata/upstream-model-eval.go.txt`; running that driver requires Magpie's full model registry and installed model files. It is not compiled into this package. No test uses a paid inference endpoint or the user's live accounts.

## Releases and packaging

`make check` runs the local Go and builtin-provider mock gates. Ordinary commits
and pull requests do not start CI. The workflows are manual validation, manual
bump, and version-tag release. `python3 scripts/bump.py patch` edits VERSION;
`--release` checks, commits, tags and pushes it. `make release` releases the
current committed version. In GitHub, **Bump and release** accepts patch, minor,
or major and dispatches the release workflow explicitly.

`make dist` builds Linux x86_64/aarch64, Windows x86_64, and macOS
x86_64/aarch64 packages with Go 1.26.8 and CGO disabled. Guard inference uses the
CPU implementation. The local gate executes Linux helpers; cross compilation
alone does not establish runtime coverage on Windows or macOS. Each archive
contains all three helpers (`zcalld-pluginctl`, `zcalld-privacy`, and
`zcalld-claude-tui`), host scripts, builtin provider modules/catalog, licenses,
and build metadata. Source and generated AUR recipes are separate assets;
SHA256SUMS covers every archive. Source staging requires a clean, committed tree.

The source AUR package is `zcalld-compat`. It installs helpers and JavaScript
resources below `/usr/lib/zcalld-compat` with CLI links in `/usr/bin`, notices below `/usr/share/doc/zcalld-compat`,
and licenses below `/usr/share/licenses/zcalld-compat`. Bun and Git are optional
runtime dependencies for JavaScript and pinned Git packages. The parent core
package supplies the native adapters. Installing this package starts no service.

The release workflow can publish the reviewed AUR recipe when its dedicated
`AUR_SSH_PRIVATE_KEY` and pinned `AUR_KNOWN_HOSTS` secrets are configured. It does
not replace published release assets. A failed draft may be completed on retry.

Builtin credentials use `import-builtin --source MAGPIE_CONFIG`. The explicit
rollback helper is `sync-auth --to-magpie MAGPIE_CONFIG`; it checks original
account fingerprints before writing refreshed credentials back. Imported Google
OAuth client metadata belongs in the private `builtin-oauth-clients.json` file,
not in the repository. Missing client setup is reported before refresh.

## Manage active accounts

`accounts list` reads the host's actual private auth file and returns IDs,
providers, labels, enabled/selected flags and credential presence. It never
returns tokens. `accounts import --provider ID --file /absolute/auth.json`
accepts an SDK auth record (api/key or oauth/access/refresh); optional `--label`
and `--account` choose metadata and a new stable ID. Existing IDs are not
silently overwritten. Vendor-native files are handled by the builtin importer
or the provider's public import/authorization session.

`accounts use ID`, `enable ID`, `disable ID` and `remove ID` update the live
registry and auth state. A numeric index from the current list is accepted.
Disabling does not move credentials: a refresh already in flight can still save
its rotated token. New provider calls check the enabled flag before egress.
Account metadata alone does not restart Bun. Unbound providers use the selected
default; the parent CLI updates explicit local bindings and reports any needed
configuration reload. Removal deletes the credential from the actual host file.

`accounts session` keeps authorization and callbacks in one host. Native Codex
credentials with an active external owner stay read-only to this component.
The explicit `handoff --account KEY --owners /absolute/owners.json` operation
verifies recorded Linux process identities have exited, re-reads the newest
credential, and changes refresh ownership without a network request. Other
platforms retain external ownership until equivalent verification is available.


## Codex response handling

The Codex adapter requests the subscription endpoint's SSE format for both
client modes. Streaming clients receive the original event bytes; nonstreaming
clients receive the complete final response object, including tool output,
usage, and extension fields.

A successful response without a Content-Type is accepted only after a complete
first Responses SSE event is validated within a 64 KiB probe. The adapter then
sets text/event-stream and replays the buffered bytes without changing them.
An explicit incompatible Content-Type, malformed first event, or exhausted
probe limit rejects the response and cancels its body. The 64 KiB limit applies
to first-event detection, not to the whole response or network traffic.
[Focused mock tests](community/builtin/tests/codex-mime.test.mjs) cover fragmented
UTF-8, BOM/CRLF framing, rejection, cancellation, and byte preservation.

The subscription request adapter removes unsupported max_output_tokens and
max_completion_tokens fields. Those fields do not provide a hard generation
token cap for this provider. Gateway deadlines and response-byte limits still
bound how long the local request is held and how much output it accepts.

## Community tools MCP

The native zcalld community extension serves installed plugins' tool exports at
`/mcp/zcalld/tools` and preserves `/mcp/magpie/tools` for migrated agents. It
reuses the existing lazy Bun host and does not open another listener. The helper
embeds the host, pi, and MCP modules as one immutable script bundle.

```sh
zcalld-pluginctl --root /absolute/extensions runtime configure
zcalld-pluginctl --root /absolute/extensions mcp show
zcalld-pluginctl --root /absolute/extensions mcp enable
zcalld-pluginctl --root /absolute/extensions mcp disable
zcalld-pluginctl --root /absolute/extensions import-mcp --source /absolute/magpie-config
```

`runtime configure` writes the private host binding and refreshes the paths and
checksums of already registered, helper-owned builtin plugins to the current
embedded bundle. It preserves account selections, enabled flags, plugin options,
unknown registry fields, and community plugins. It does not add builtin entries,
read Magpie state, start plugins, or refresh accounts. Run it after upgrading
this package, then restart the native gateway; older bundles remain available
to running workers.

The registry is replaced atomically after the new bundle and host binding are
prepared. These files are not one transaction: a later write failure can leave
a new cache or host binding, while the existing registry remains intact. The
[upgrade tests](community/runtime_test.go) cover selection, preserved state,
idempotence, and validation failures.

`import-magpie` includes MCP state automatically; `import-mcp`
updates an existing staged registry. Only permission metadata is copied into the
derived MCP map, never gateway credentials. Unknown or unsupported nonempty
budgets are refused rather than relaxed. Tool grants, issuer intersections,
readonly/off/expiry, and FIFO concurrency restrictions are preserved.

See [NOTICE_MCP.md](NOTICE_MCP.md) for the pinned upstream contract and licensing,
and the [zcalld MCP guide](https://github.com/LIghtJUNction/zcalld/blob/main/docs/MCP.md)
for endpoint behavior and native integration tests. `make check` includes both
MCP and routing worker tests. No fixture uses a live account or provider.

## Explicit quota updates

`zcalld plugins quota list --json` reads local capabilities and cached allowance windows only.
`zcalld plugins quota refresh --account ACCOUNT_ID --json` explicitly updates one supported, enabled community account through its existing usage and renewal hooks. Credentials stay in the helper; imported quota files are preserved. Unsupported or externally managed accounts are not offered an invented refresh capability. See [the quota contract](https://github.com/LIghtJUNction/zcalld-compat/blob/main/docs/QUOTAS.md) for cancellation, private caching and JSON output.

## Optional native Codex metadata

With zcalld 0.4.0 and this component 0.2.0 or newer, the native community
extension can serve Codex 0.170's read-only model catalog, usage, account
bootstrap and config bundle requests. This is disabled by default and reuses
the lazy host. It requires an explicit local provider/account binding; remote
callers also require a verified caller-key binding.

```sh
zcalld-pluginctl --root /absolute/extensions codex configure --file /absolute/bindings.json
zcalld-pluginctl --root /absolute/extensions runtime configure
zcalld-pluginctl --root /absolute/extensions codex show
zcalld-pluginctl --root /absolute/extensions codex disable
```

The binding object has `enabled`, optional `loopback`, and optional `callers`
fields. Each binding contains an existing local `provider` ID and nonempty
`routes` list. Caller entries use gateway caller-key IDs, never their secrets.
Configuration is bounded, validated and saved atomically; invalid input leaves
the registry unchanged. These commands do not start Bun, refresh accounts or
restart the gateway.

The metadata hook uses a fixed official origin and four read-only operations.
It does not invoke loaders, generation or token refresh. External credentials
remain read-only. Instructions come from the bound account's upstream catalog;
only explicit static route-to-slug aliases are exposed. Validated raw usage,
bootstrap and config JSON is preserved. A request deadline includes shared
initialization; canceling one request does not terminate the shared host.
Native initialization fails closed if an enabled plugin cannot load; the
management CLI retains partial-load diagnostics.

See [the gateway configuration and endpoint contract](https://github.com/LIghtJUNction/zcalld/blob/main/docs/CODEX.md).
Tests use synthetic credentials. The ordinary Responses adapter also retains
opaque compaction IDs and encrypted content, with plaintext privacy processing
still enabled.

### Codex subscription-credit policy

`zcalld plugins codex policy plan --source ABS_MAGPIE_CONFIG --json` reads the
current source's `codexNoCredits` identities and matches only proven imported
accounts. It reports counts and a plan hash without writing files. Apply the
reviewed plan with `codex policy sync --source ABS_MAGPIE_CONFIG
--expect-plan HASH --json`. Changed or unmapped identities stop the operation.
The command modifies only private registry `account_policies`; credentials,
refresh ownership, and credential rollback fingerprints are untouched.

The native host resolves the actual account first and rereads its policy on each
generation request, even with cached loaders. An explicit `codex_no_credits:
true` policy checks the fixed official account-usage endpoint with an eight-second
ceiling, a 64 KiB bound, no redirects, and cancellation shared only while another
caller still needs the lookup. A one-second bounded cache is keyed by the
official account identity. Exhausted subscription windows return 429 before
generation, and unverified availability returns 503. Existing group fallback
can select another account; the blocked account is not retried using credits.
Malformed present policy is an error. Unrestricted accounts add no usage query.

The source can acquire new policies after migration; synchronization remains
explicit and does not reimport auth. There is no server-side guarantee that
credits can never be charged: another client or the current generation can cross
a window after the check. See the [core contract and usage guide](https://github.com/LIghtJUNction/zcalld/blob/main/docs/CODEX.md#subscription-credit-policy)
for the exact scope. These changes require the updated native host as declared
by the package dependency; installing a helper alone does not rewrite an older
materialized builtin bundle.

### Optional Claude Code maintenance

The [Claude maintenance guide](docs/CLAUDE-MAINTENANCE.md) covers explicit
current or saved native-profile bindings, off/week/all schedules, multiple local
clock times, imported warmup history, and manual single/all-account warmup.
The native Claude CLI owns its credentials; zcalld does not copy its OAuth tokens
or call Anthropic's OAuth endpoints itself. The bundled `zcalld-claude-tui` runs
a real PTY/ConPTY session and does not silently fall back to print mode.

Installation and plan/apply leave scheduling disabled. A warmup can consume
subscription allowance; the no-extra setting and configured extra-usage caps
are checked before sending. Native CLI startup can itself have effects, so an
uncertain outcome stays pending for review and is never blindly replayed.
Claude reset-card operations are unsupported. Maintenance is separate from the
gateway provider adapter and does not register a provider route by itself.
