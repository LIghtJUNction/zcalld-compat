package community

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ImportOptions struct{ Source, PrivacyRoot, ModelRoot string }

func copyPrivateTree(source, target string) (int, int64, error) {
	count := 0
	var total int64
	err := filepath.WalkDir(source, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(source, p)
		if e != nil {
			return e
		}
		to := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			resolved, e := filepath.EvalSymlinks(p)
			if e != nil {
				return e
			}
			r, e := filepath.Rel(source, resolved)
			if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return errors.New("plugin symlink leaves its source tree")
			}
			mapped := filepath.Join(target, r)
			link, e := filepath.Rel(filepath.Dir(to), mapped)
			if e != nil {
				return e
			}
			return os.Symlink(link, to)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("plugin import encountered a special file")
		}
		total += info.Size()
		if total > 4<<30 {
			return errors.New("plugin/model import exceeds 4 GiB")
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		mode := os.FileMode(0600)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0700
		}
		out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			in.Close()
			return e
		}
		_, e = io.Copy(out, in)
		in.Close()
		ce := out.Close()
		if e == nil {
			e = ce
		}
		count++
		return e
	})
	return count, total, err
}
func ImportMagpie(root string, o ImportOptions) (map[string]any, error) {
	if !filepath.IsAbs(o.Source) || !filepath.IsAbs(o.PrivacyRoot) {
		return nil, errors.New("import source/privacy paths must be absolute")
	}
	source, e := filepath.EvalSymlinks(o.Source)
	if e != nil {
		return nil, e
	}
	if rel, e := filepath.Rel(source, root); e == nil && (rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")) {
		return nil, errors.New("import destination must be outside Magpie")
	}
	unlock, e := Lock(root)
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	if len(r.Plugins) > 0 {
		return nil, errors.New("destination extensions are nonempty; keep the prior snapshot and import into a new directory")
	}
	if _, e = os.Stat(filepath.Join(o.PrivacyRoot, "settings.json")); e == nil {
		return nil, errors.New("destination privacy settings already exist")
	}
	var list struct {
		Plugins []struct {
			Spec    string         `json:"spec"`
			Off     bool           `json:"off"`
			Options map[string]any `json:"options"`
		} `json:"plugins"`
		Config map[string]any    `json:"config"`
		Prefer map[string]string `json:"prefer"`
	}
	b, e := readLimit(filepath.Join(source, "plugins.json"), 8<<20)
	if e != nil {
		return nil, e
	}
	if json.Unmarshal(b, &list) != nil {
		return nil, errors.New("Magpie plugin list is invalid")
	}
	var settings map[string]any
	b, e = readLimit(filepath.Join(source, "settings.json"), 8<<20)
	if e != nil {
		return nil, e
	}
	if json.Unmarshal(b, &settings) != nil {
		return nil, errors.New("Magpie settings are invalid")
	}
	stage, e := os.MkdirTemp(root, ".magpie-import-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(stage)
	generation := filepath.Join(root, "imported-magpie")
	if _, e = os.Stat(generation); e == nil {
		return nil, errors.New("destination imported plugin tree already exists")
	}
	copied, bytes, e := copyPrivateTree(filepath.Join(source, "plugins"), filepath.Join(stage, "plugins"))
	if e != nil {
		return nil, e
	}
	r.Config = list.Config
	r.Prefer = list.Prefer
	if options, ok := settings["pluginOptions"].(map[string]any); ok {
		r.ProviderOptions = options
	}
	names := map[string]bool{}
	for _, p := range list.Plugins {
		spec := strings.TrimPrefix(p.Spec, "npm:")
		name := baseName(spec)
		if !ValidName(name) || names[name] {
			return nil, errors.New("imported plugin name is ambiguous")
		}
		names[name] = true
		from := filepath.Join(stage, "plugins", "node_modules", npmName(spec))
		if _, e = os.Stat(from); e != nil {
			return nil, fmt.Errorf("installed package %s was not found; import explicit local/Git sources separately", name)
		}
		version, gateway := packageInfo(from)
		hash, e := digest(from)
		if e != nil {
			return nil, e
		}
		rel, _ := filepath.Rel(stage, from)
		target := filepath.Join(generation, rel)
		gw := ""
		if gateway != "" {
			grel, _ := filepath.Rel(stage, gateway)
			gw = filepath.Join(generation, grel)
		}
		pinned := "npm:" + npmName(spec) + "@" + version
		r.Plugins = append(r.Plugins, Entry{Name: name, Path: target, Kind: "community", Enabled: !p.Off, Version: version, Source: pinned, Revision: hash, SHA256: hash, Gateway: gw, Options: p.Options})
	}
	auth, e := readLimit(filepath.Join(source, "plugin-auth.json"), 8<<20)
	if errors.Is(e, os.ErrNotExist) {
		auth = []byte("{}")
	} else if e != nil {
		return nil, e
	}
	var accounts map[string]json.RawMessage
	if json.Unmarshal(auth, &accounts) != nil {
		return nil, errors.New("Magpie plugin auth is invalid")
	}
	mask := map[string]any{}
	for k, v := range settings {
		if strings.HasPrefix(k, "redact") || strings.HasPrefix(k, "mask") {
			mask[k] = v
		}
	}
	key, e := readLimit(filepath.Join(source, "redact.key"), 4096)
	if errors.Is(e, os.ErrNotExist) {
		key, e = readLimit(filepath.Join(source, "masking", "redact.key"), 4096)
	}
	if e != nil || len(key) < 32 {
		return nil, errors.New("Magpie privacy key is unavailable")
	}
	if e = os.MkdirAll(o.PrivacyRoot, 0700); e != nil {
		return nil, e
	}
	historyFiles := 0
	if _, e := os.Stat(filepath.Join(source, "masking")); e == nil {
		n, _, e := copyPrivateTree(filepath.Join(source, "masking"), filepath.Join(o.PrivacyRoot, "history"))
		if e != nil {
			return nil, e
		}
		historyFiles = n
	}
	modelRoot := o.ModelRoot
	if custom, ok := settings["redactModelDir"].(string); ok && custom != "" {
		modelRoot = custom
	}
	modelFiles := 0
	var modelBytes int64
	if modelRoot != "" {
		for _, kind := range []string{"astrlink-guard", "privacy-filter"} {
			from := filepath.Join(modelRoot, kind)
			if _, e = os.Stat(from); os.IsNotExist(e) {
				continue
			} else if e != nil {
				return nil, e
			}
			count, n, e := copyPrivateTree(from, filepath.Join(o.PrivacyRoot, "models", kind))
			if e != nil {
				return nil, e
			}
			modelFiles += count
			modelBytes += n
		}
		mask["redactModelDir"] = filepath.Join(o.PrivacyRoot, "models")
	}
	maskBytes, e := json.MarshalIndent(mask, "", "  ")
	if e != nil {
		return nil, e
	}
	if e = Atomic(filepath.Join(o.PrivacyRoot, "redact.key"), key, 0600); e != nil {
		return nil, e
	}
	if e = Atomic(filepath.Join(o.PrivacyRoot, "settings.json"), maskBytes, 0600); e != nil {
		return nil, e
	}
	if e = Atomic(filepath.Join(root, "plugin-auth.json"), auth, 0600); e != nil {
		return nil, e
	}
	if b, e := readLimit(filepath.Join(source, "plugin-providers.json"), 8<<20); e == nil {
		if e = Atomic(filepath.Join(root, "plugin-providers.json"), b, 0600); e != nil {
			return nil, e
		}
	}
	if e = os.Rename(stage, generation); e != nil {
		return nil, e
	}
	if e = Save(root, r); e != nil {
		return nil, e
	}
	host, pi, e := Scripts(root)
	if e != nil {
		return nil, e
	}
	bun, e := exec.LookPath("bun")
	if e != nil {
		return nil, errors.New("import completed; Bun is missing and community plugins remain unavailable")
	}
	communityConfig := map[string]any{"bun": bun, "host": host, "pi": pi, "registry": filepath.Join(root, "registry.json"), "auth_path": filepath.Join(root, "plugin-auth.json")}
	b, _ = json.MarshalIndent(communityConfig, "", "  ")
	if e = Atomic(filepath.Join(root, "host.json"), b, 0600); e != nil {
		return nil, e
	}
	exe, _ := os.Executable()
	worker := filepath.Join(filepath.Dir(exe), "zcalld-privacy")
	privacyConfig := map[string]any{"worker": worker, "settings": filepath.Join(o.PrivacyRoot, "settings.json"), "key": filepath.Join(o.PrivacyRoot, "redact.key"), "model_dir": filepath.Join(o.PrivacyRoot, "models")}
	b, _ = json.MarshalIndent(privacyConfig, "", "  ")
	if e = Atomic(filepath.Join(o.PrivacyRoot, "plugin.json"), b, 0600); e != nil {
		return nil, e
	}
	return map[string]any{"ok": true, "plugins": len(r.Plugins), "plugin_accounts": len(accounts), "copied_files": copied, "copied_bytes": bytes, "history_files": historyFiles, "model_files": modelFiles, "model_bytes": modelBytes, "builtin_oauth_imported": false, "community_config": filepath.Join(root, "host.json"), "privacy_config": filepath.Join(o.PrivacyRoot, "plugin.json")}, nil
}
