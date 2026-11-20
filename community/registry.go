// Package community manages explicitly trusted local and community extensions.
package community

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Entry struct {
	Name       string         `json:"name"`
	Path       string         `json:"path"`
	Kind       string         `json:"kind"`
	Enabled    bool           `json:"enabled"`
	Version    string         `json:"version,omitempty"`
	Source     string         `json:"source"`
	Revision   string         `json:"revision,omitempty"`
	SHA256     string         `json:"sha256"`
	Gateway    string         `json:"gateway,omitempty"`
	Options    map[string]any `json:"options,omitempty"`
	ConfigFile string         `json:"config_file,omitempty"`
}
type Registry struct {
	MCPToolsEnabled *bool                    `json:"mcp_tools_enabled,omitempty"`
	MCPPermissions  map[string]MCPPermission `json:"mcp_permissions,omitempty"`
	Accounts        map[string]AccountMeta   `json:"accounts,omitempty"`
	AccountDefaults map[string]string        `json:"account_defaults,omitempty"`
	Version         int                      `json:"version"`
	Plugins         []Entry                  `json:"plugins"`
	Config          map[string]any           `json:"config,omitempty"`
	Prefer          map[string]string        `json:"prefer,omitempty"`
	ProviderOptions map[string]any           `json:"provider_options,omitempty"`
	ModelsDevPath   string                   `json:"models_dev_path,omitempty"`
}

func (e Entry) Public() map[string]any {
	return map[string]any{"name": e.Name, "path": e.Path, "kind": e.Kind, "enabled": e.Enabled, "version": e.Version, "source": e.Source, "revision": e.Revision, "sha256": e.SHA256, "configured": e.ConfigFile != "" || len(e.Options) > 0}
}
func Read(root string) (Registry, error) {
	r := Registry{Version: 1, Plugins: []Entry{}}
	b, e := readLimit(filepath.Join(root, "registry.json"), 8<<20)
	if errors.Is(e, os.ErrNotExist) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	if json.Unmarshal(b, &r) != nil || r.Version != 1 {
		return r, errors.New("unsupported or invalid extension registry")
	}
	return r, nil
}
func readLimit(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file exceeds extension limit")
	}
	return b, nil
}
func Atomic(path string, b []byte, mode os.FileMode) error {
	if i, e := os.Lstat(path); e == nil && i.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace a symlink")
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	if e := privatefile.Protect(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".zcalld-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = privatefile.Protect(name, mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return privatefile.SyncDir(filepath.Dir(path))
}
func Save(root string, r Registry) error {
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	return Atomic(filepath.Join(root, "registry.json"), append(b, '\n'), 0600)
}
func Lock(root string) (func(), error) {
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	unlock, e := platformLock(f)
	if e != nil {
		f.Close()
		return nil, e
	}
	return func() { unlock(); f.Close() }, nil
}

var namePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")

func ValidName(s string) bool { return namePattern.MatchString(s) }
func index(r Registry, name string) int {
	for i, p := range r.Plugins {
		if p.Name == name {
			return i
		}
	}
	return -1
}
func List(root string) (map[string]any, error) {
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	out := make([]any, 0, len(r.Plugins))
	for _, p := range r.Plugins {
		out = append(out, p.Public())
	}
	return map[string]any{"plugins": out}, nil
}
func Inspect(root, name string) (map[string]any, error) {
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	i := index(r, name)
	if i < 0 {
		return nil, errors.New("extension not found")
	}
	return map[string]any{"plugin": r.Plugins[i].Public()}, nil
}
func Change(root, name, action, file string) (map[string]any, error) {
	unlock, e := Lock(root)
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	i := index(r, name)
	if i < 0 {
		return nil, errors.New("extension not found")
	}
	p := &r.Plugins[i]
	switch action {
	case "enable":
		if p.Kind == "native" {
			sum, e := fileHash(p.Path)
			if e != nil || sum != p.SHA256 {
				return nil, errors.New("native extension checksum mismatch")
			}
		}
		if p.Kind == "community" {
			if e := configureRuntimeHost(root); e != nil {
				return nil, e
			}
		}
		p.Enabled = true
	case "disable":
		p.Enabled = false
	case "remove":
		r.Plugins = append(r.Plugins[:i], r.Plugins[i+1:]...)
	case "config":
		b, e := readLimit(file, 1<<20)
		if e != nil {
			return nil, e
		}
		if p.Kind == "native" {
			dest := filepath.Join(root, "config", name+".conf")
			if e = Atomic(dest, b, 0600); e != nil {
				return nil, e
			}
			p.ConfigFile = dest
		} else {
			var options map[string]any
			if json.Unmarshal(b, &options) != nil {
				return nil, errors.New("community options must be one JSON object")
			}
			p.Options = options
		}
	default:
		return nil, errors.New("unknown extension change")
	}
	if e = Save(root, r); e != nil {
		return nil, e
	}
	if action == "remove" {
		// Only remove our named installation directory; never remove imported files.
		own := filepath.Join(root, "packages", name)
		if ValidName(name) {
			if e = os.RemoveAll(own); e != nil {
				return nil, e
			}
		}
		return map[string]any{"ok": true, "name": name}, nil
	}
	return map[string]any{"ok": true, "plugin": p.Public()}, nil
}
func digest(root string) (string, error) {
	var files []string
	e := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("plugin source contains a symlink")
		}
		if !d.Type().IsRegular() {
			return errors.New("plugin source contains a non-regular file")
		}
		files = append(files, p)
		return nil
	})
	if e != nil {
		return "", e
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintln(h, filepath.ToSlash(rel))
		f, e := os.Open(p)
		if e != nil {
			return "", e
		}
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil {
			return "", e
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func fileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func baseName(source string) string {
	s := strings.TrimSuffix(source, ".git")
	s = strings.TrimSuffix(s, ".so")
	s = strings.TrimPrefix(s, "npm:")
	s = strings.TrimPrefix(s, "github:")
	s = filepath.Base(s)
	if i := strings.LastIndex(s, "@"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "lib")
	s = strings.TrimPrefix(s, "zcalld_")
	return s
}
