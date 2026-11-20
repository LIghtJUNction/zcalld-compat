package community

import (
	"errors"
	"time"
)

// A provider name or cached capability is not a code identity. This read-only
// check accepts only the helper's complete current embedded builtin bundle.
// It never materializes scripts, discovers providers, or reads native auth.
func verifiedBuiltinCodex(root string, r Registry) (Entry, string) {
	var entry Entry
	count := 0
	for _, p := range r.Plugins {
		if p.Name == "builtin-codex" || p.Source == "builtin-codex" {
			count++
			entry = p
		}
	}
	if count != 1 || entry.Name != "builtin-codex" || entry.Source != "builtin-codex" ||
		entry.Kind != "community" || entry.Gateway != "" {
		return Entry{}, "plugin_unavailable"
	}
	if !entry.Enabled {
		return Entry{}, "plugin_disabled"
	}
	if preferred := r.Prefer["builtin:codex"]; preferred != "" && preferred != entry.Source {
		return Entry{}, "plugin_unavailable"
	}
	if err := verifyBuiltinReference(root, entry.Path, entry.SHA256, true); err != nil {
		if errors.Is(err, errBuiltinOutdated) {
			return Entry{}, "builtin_runtime_outdated"
		}
		return Entry{}, "plugin_unavailable"
	}
	return entry, ""
}

// Both usage and maintenance may borrow this exact builtin host. The pinned
// launch rechecks the registry and cannot load another same-named package.
func startBuiltinCodexHost(root string, operationDeadline, drainDeadline time.Time) (*Host, error) {
	registry, err := Read(root)
	if err != nil {
		return nil, err
	}
	entry, reason := verifiedBuiltinCodex(root, registry)
	if reason != "" {
		return nil, errors.New("the verified builtin Codex adapter is unavailable")
	}
	host, err := startPinnedHostBudget(root, entry, operationDeadline, drainDeadline)
	if err != nil {
		return nil, err
	}
	current, err := Read(root)
	if err == nil {
		next, reason := verifiedBuiltinCodex(root, current)
		if reason == "" && sameHostEntry(entry, next) {
			return host, nil
		}
	}
	host.Close()
	return nil, errors.New("the builtin Codex adapter changed while starting")
}
