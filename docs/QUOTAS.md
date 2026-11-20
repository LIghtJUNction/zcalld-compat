# Explicit quota updates

The native gateway does not poll subscription quotas. The optional compatibility
helper can read a local quota cache or explicitly ask one supported community
account for its current allowance.

```sh
zcalld plugins quota list --json
zcalld plugins quota refresh --account ACCOUNT_ID --json
```

`list` reads the extension registry, account metadata, provider capabilities and
the helper's quota cache. It does not start Bun, run plugin hooks, renew a token,
or contact a provider. Account identifiers are opaque selectors; credentials are
never command arguments or output fields. The desktop uses this local operation
when its panel opens or its ordinary refresh button is pressed.

`refresh` is an explicit network operation. It accepts one enabled account of
one enabled community plugin whose cached metadata declares `auth.usage`.
The host then checks the actual hook and follows its existing
`usage → fresh → inScope` path. Token renewal and saving remain with that host's
account-scoped renewal fence. Only the selected plugin module is initialized.
There is no automatic retry or background polling.

Unknown capabilities, disabled accounts or plugins, ambiguous module matches and
unsupported usage hooks are not presented as successful updates. Arbitrary
community hooks are not allowed to take over an account marked
`refreshOwner: external`; that account remains with its native credential owner.
The current built-in OAuth adapters do not implement `auth.usage`, so their
imported quota readings remain cached readings, without a pretend refresh button.

## Capability and cache state

The imported `plugin-providers.json` is preserved. An explicit
`zcalld plugins providers --json` discovery also saves a small
`quota-capabilities.json` containing only provider IDs, plugin specs and the
usage capability. Provider discovery may contact its providers; it is never
performed implicitly by `quota list` or a panel snapshot.

A successful update atomically merges the selected account into the private
`extensions/quota-cache.json` (mode 0600, or the existing private Windows ACL).
A registry lock protects concurrent cache merges, and the account and plugin
are rechecked before committing the result. Other accounts' cached readings and
the original `quotas.json` are preserved.

Provider errors and timeouts leave the previous successful cache intact. Error
codes are static; raw plugin errors, tokens, user identities returned by the
provider and arbitrary result fields are not forwarded to the UI. The cache
retains only bounded allowance-window fields: percentages, reset times,
numeric amount/limit/balance, unit and window labels. A missing percentage is
unknown, not zero usage or a full allowance.

The RPC has a 30-second total waiting deadline. On timeout the helper sends an
abort and closes stdin, then allows the existing 35-second shutdown window for
an in-progress credential checkpoint before terminating its own host. A desktop
operation may therefore take longer than 30 seconds to return safely. It does
not restart the gateway.

## JSON contract

`quota list` returns `schema_version: 1`, `local_only: true`,
`accounts` and `readings`. Each account has
`id/provider/label/enabled/can_refresh/reason/cached_at`.
Each reading has `account/provider/label/at/windows`.

`quota refresh` returns `ok: true`, the selected account/provider IDs,
the cache timestamp and window count. A rejected or failed provider operation
returns `ok: false`, a static `error` code and `cache_preserved: true`.
Storage or configuration errors can fail the command before a success is
confirmed; callers should reread the local cache instead of automatically
reissuing the provider request.
