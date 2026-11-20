package community

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var builtinBundleName = regexp.MustCompile("^[0-9a-f]{24}$")

var builtinEntryFiles = map[string]string{
	"builtin-codex":       "codex.mjs",
	"builtin-copilot":     "copilot.mjs",
	"builtin-gemini":      "codeassist.mjs",
	"builtin-antigravity": "antigravity.mjs",
}

// ConfigureRuntime updates the host binding and only existing helper-owned
// builtin targets. It never imports accounts, enables plugins, or starts Bun.
// Keep the registry lock here; Change already owns it and calls the host-only
// helper so enabling a community plugin cannot recursively acquire this lock.
func ConfigureRuntime(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	unlock, err := Lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	next, err := refreshedBuiltinRegistry(root)
	if err != nil {
		return err
	}
	if err := configureRuntimeHost(root); err != nil {
		return err
	}
	if next == nil {
		return nil
	}
	return Atomic(filepath.Join(root, "registry.json"), next, 0600)
}

func rawString(value json.RawMessage) string {
	var text string
	_ = json.Unmarshal(value, &text)
	return text
}

func builtinOwnedTarget(root string, entry map[string]json.RawMessage) string {
	name := rawString(entry["name"])
	file, ok := builtinEntryFiles[name]
	if !ok || rawString(entry["kind"]) != "community" || rawString(entry["source"]) != name {
		return ""
	}
	path := rawString(entry["path"])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ""
	}
	relative, err := filepath.Rel(filepath.Join(root, "runtime", "builtin"), path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 2 || !builtinBundleName.MatchString(parts[0]) || parts[1] != file {
		return ""
	}
	return file
}

// RawMessage preserves unknown fields and exact number lexemes, including
// account and community plugin state that the current helper does not know.
func refreshedBuiltinRegistry(root string) ([]byte, error) {
	path := filepath.Join(root, "registry.json")
	before, err := readLimit(path, 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var registry map[string]json.RawMessage
	var version int
	var entries []map[string]json.RawMessage
	if json.Unmarshal(before, &registry) != nil ||
		json.Unmarshal(registry["version"], &version) != nil || version != 1 ||
		json.Unmarshal(registry["plugins"], &entries) != nil {
		return nil, errors.New("unsupported or invalid extension registry")
	}
	// Validate all registered builtin candidates before materializing anything.
	// A same-named external path is not an owned adapter. Unknown/edited bundles
	// are rejected without changing registry, credentials or the host binding.
	for _, entry := range entries {
		_, nameKnown := builtinEntryFiles[rawString(entry["name"])]
		_, sourceKnown := builtinEntryFiles[rawString(entry["source"])]
		if !nameKnown && !sourceKnown {
			continue
		}
		if builtinOwnedTarget(root, entry) == "" {
			return nil, errBuiltinUnverified
		}
		if err := verifyBuiltinReference(root, rawString(entry["path"]), rawString(entry["sha256"]), false); err != nil {
			return nil, err
		}
	}
	var resources map[string]string
	changed := false
	for _, entry := range entries {
		file := builtinOwnedTarget(root, entry)
		if file == "" {
			continue
		}
		if resources == nil {
			if err := safeBuiltinDestination(root); err != nil {
				return nil, err
			}
			resources, err = BuiltinScripts(root)
			if err != nil {
				return nil, err
			}
		}
		target := resources[file]
		if target == "" {
			return nil, errors.New("registered builtin adapter is unavailable")
		}
		data, err := builtinFiles.ReadFile("builtin/" + file)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		sum := hex.EncodeToString(digest[:])
		if rawString(entry["path"]) == target && rawString(entry["sha256"]) == sum {
			continue
		}
		entry["path"], _ = json.Marshal(target)
		entry["sha256"], _ = json.Marshal(sum)
		changed = true
	}
	if !changed {
		return nil, nil
	}
	registry["plugins"], err = json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	next, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(next, '\n'), nil
}
