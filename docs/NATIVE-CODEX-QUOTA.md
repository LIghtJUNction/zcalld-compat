# Native Codex quota reads and builtin runtime upgrades

The explicit quota action can read an existing native Codex login without taking ownership of its refresh token. This capability is limited to the verified builtin Codex adapter; a community module with the same name is not sufficient.

## Read-only account usage

`zcalld plugins quota list --config /absolute/path/config.conf` reads local registry and cache state. It does not start Bun, read the native credential file, or contact a provider. A native Codex account is marked refreshable only when it is explicitly enabled and the registered adapter matches the helper's complete embedded builtin bundle.

The explicit action is:

```sh
zcalld plugins quota refresh --account OPAQUE_ACCOUNT_KEY --config /absolute/path/config.conf
```

The action uses the selected account's existing access token for one GET to the fixed official account-usage endpoint. It does not invoke authentication refresh, provider loaders, chat hooks, generation, or reset-card operations. Redirects are rejected, the operation shares a 30-second deadline including host initialization, and the response is limited to 64 KiB. Host cleanup retains the existing bounded drain allowance.

External credentials must still identify the bound account and be unexpired. A credential or account configuration change during the operation rejects the result. Failure preserves the previous cached reading. The returned data contains only the canonical primary/secondary windows and their reported durations and reset times; credits balances are not relabeled as currency. A successful fresh cache write may produce an enabled desktop quota observation. Reading the cache never produces a notification.

## Upgrade an existing builtin runtime

After installing the new helper, use the existing command:

```sh
zcalld plugins runtime configure --config /absolute/path/config.conf
```

The equivalent direct helper command on Arch is:

```sh
/usr/lib/zcalld-compat/zcalld-pluginctl --root /absolute/path/extensions runtime configure
```

This updates the host binding and registered builtin script paths/checksums to the helper's embedded content-addressed bundle. It does not reimport logins, start Bun, make network requests, enable accounts, or modify authentication files. Account keys, disabled states, selected defaults, policies, and unknown registry fields are preserved. The command reports that the managed gateway needs a normal restart before an already running host uses the new binding.

Automatic upgrade accepts the current embedded bundle and the complete known official [v0.1.6 source](https://github.com/LIghtJUNction/zcalld-compat/tree/e4dffde8012a9120df00d2b7b93e19933007a19f/community/builtin). The historical manifest covers all nine runtime files, not only the entry module. The verifier checks the exact content address, member list, file sizes, hashes, private ownership/permissions, and absence of symbolic links. Edited bundles, extra members, arbitrary same-named modules, unrecognized historical bundles, and an edited destination are rejected without replacing registry or credential state. A self-consistent directory hash alone does not establish official provenance.

This command does not change any native Codex configuration. It should be used instead of reimporting old login snapshots when credentials may have rotated.

## Validation scope

Synthetic tests cover the actual Go helper to Bun host path, explicit account selection, disabled and mismatched accounts, expired credentials, redirects, response limits, cancellation, and a credential change during the GET. The runtime upgrade tests use the exact public v0.1.6 bundle and verify preserved state, rejection of edited/unsafe nodes, and idempotence. They do not make real provider or model requests.

Windows compilation validates the platform ownership-check implementation; it does not substitute for a native Windows runtime test. The repository's release gates provide the separate native-platform checks.
