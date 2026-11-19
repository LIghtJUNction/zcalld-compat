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

The parent zcalld workspace builds the native `redact` and `community` adapters through zplugin. The installation layout places executables in `/usr/libexec/zcalld` and native plugins in `/usr/lib/zcalld/plugins`. Bun is needed only for JavaScript plugins; Git is needed only for Git installation sources.

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

## Public plugin operations

`invoke PROVIDER EVENT --input-file /absolute/request.json` calls an allowlisted public host method. Raw authentication exporters and loader results are unavailable through this command. Public results remove credential fields.

OAuth callbacks must run in the same host as authorization. `session` keeps that host alive and accepts JSON lines with `id`, `method`, and `params` on stdin, returning a corresponding sanitized result. It supports authorization, callback, prompts, validation, usage, check-in, account checks, tools, and search. End stdin to stop that helper and its host.

## Validation scope

The privacy wrapper tests cover block behavior, setting reloads, the reversible disable switch, explicit-model failure, actual local-model classification, JSON tool arguments, and split SSE placeholders. Manager tests cover lifecycle changes, immutable Git pins, checksum tampering, copy boundaries, and suppressed package scripts. Parent gateway tests exercise real native libraries and Bun against loopback mock providers.

Upstream model evaluation files that require the full Magpie download/cache subsystem are retained under the `magpie_model_corpus` build tag. The ordinary engine and Guard tests run without that subsystem. No test uses a paid inference endpoint or the user's live accounts.

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
contains both helpers, host scripts, builtin provider modules/catalog, licenses,
and build metadata. Source and generated AUR recipes are separate assets;
SHA256SUMS covers every archive. Source staging requires a clean, committed tree.

The source AUR package is `zcalld-compat`. It installs helpers and JavaScript
resources below `/usr/libexec/zcalld`, notices below `/usr/share/doc/zcalld-compat`,
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
