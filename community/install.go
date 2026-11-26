package community

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type InstallOptions struct {
	Source, Name, Revision, SHA256, Version string
	Update                                  bool
}

var commitPattern = regexp.MustCompile("^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$")
var npmVersion = regexp.MustCompile("^(@[A-Za-z0-9_.-]+/)?[A-Za-z0-9_.-]+@[0-9]+[.][0-9]+[.][0-9]+([+-][A-Za-z0-9._-]+)?$")

func run(cwd string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	// Package manager output can contain private registry credentials.
	// Discard it and return a fixed command-level error.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return fmt.Errorf("%s failed; installation was not changed", filepath.Base(args[0]))
	}
	return nil
}
func copyTree(from, to string) error {
	var total int64
	return filepath.WalkDir(from, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(from, p)
		if e != nil {
			return e
		}
		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, rel), 0700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("local source symlinks are not copied")
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("local source contains non-regular files")
		}
		total += info.Size()
		if total > 128<<20 {
			return errors.New("local plugin source exceeds 128 MiB")
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		out, e := os.OpenFile(filepath.Join(to, rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0755)
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
		return e
	})
}
func npmName(spec string) string {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i]
	}
	return spec
}
func packageInfo(path string) (version, gateway string) {
	b, e := readLimit(filepath.Join(path, "package.json"), 1<<20)
	if e != nil {
		return
	}
	var p struct {
		Version string `json:"version"`
		Magpie  struct {
			Hooks string `json:"hooks"`
		} `json:"magpie"`
	}
	if decodeJSON(b, &p) != nil {
		return
	}
	version = p.Version
	if p.Magpie.Hooks != "" {
		g := filepath.Join(path, p.Magpie.Hooks)
		rel, e := filepath.Rel(path, g)
		if e == nil && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			if i, e := os.Stat(g); e == nil && i.Mode().IsRegular() {
				gateway = g
			}
		}
	}
	return
}
func Install(root string, o InstallOptions) (map[string]any, error) {
	unlock, e := Lock(root)
	if e != nil {
		return nil, e
	}
	defer unlock()
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	if o.Name == "" {
		o.Name = baseName(o.Source)
	}
	if !ValidName(o.Name) {
		return nil, errors.New("extension name must use 1..64 letters, digits, dots, underscores or hyphens")
	}
	old := index(r, o.Name)
	if old >= 0 && !o.Update {
		return nil, errors.New("extension exists; use update with an explicit revision or version")
	}
	if old < 0 && o.Update {
		return nil, errors.New("extension not found")
	}
	if e = os.MkdirAll(filepath.Join(root, "packages", o.Name), 0700); e != nil {
		return nil, e
	}
	stage, e := os.MkdirTemp(filepath.Join(root, "packages", o.Name), ".stage-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(stage)
	source := o.Source
	if source == "" && old >= 0 {
		source = r.Plugins[old].Source
	}
	kind := "community"
	target := stage
	revision := ""
	switch {
	case strings.HasPrefix(source, "magpie:"):
		if o.Version != "" {
			return nil, errors.New("resolve the exact Magpie distribution source before changing versions")
		}
		target, e = installMagpiePackage(stage, source)
		if e != nil {
			return nil, e
		}
	case strings.HasPrefix(source, "npm:"):
		spec := strings.TrimPrefix(source, "npm:")
		if o.Version != "" {
			spec = npmName(spec) + "@" + o.Version
		}
		if !npmVersion.MatchString(spec) {
			return nil, errors.New("npm plugins require an exact version: npm:package@1.2.3")
		}
		if e = Atomic(filepath.Join(stage, "package.json"), []byte("{\"private\":true}"), 0600); e != nil {
			return nil, e
		}
		if e = run(stage, "bun", "add", "--ignore-scripts", "--no-progress", "--exact", spec); e != nil {
			return nil, e
		}
		target = filepath.Join(stage, "node_modules", npmName(spec))
		source = "npm:" + spec
	case strings.HasPrefix(source, "github:") || strings.HasPrefix(source, "https://") || (strings.Count(source, "/") == 1 && !strings.HasPrefix(source, "/") && func() bool { _, e := os.Stat(source); return os.IsNotExist(e) }()):
		remote := strings.TrimPrefix(source, "github:")
		if !strings.HasPrefix(remote, "https://") {
			remote = "https://github.com/" + strings.TrimSuffix(remote, ".git") + ".git"
		}
		if strings.Contains(remote, "@") || strings.Contains(remote, "?") || strings.Contains(remote, "#") {
			return nil, errors.New("Git source must be a credential-free HTTPS URL")
		}
		if !commitPattern.MatchString(o.Revision) {
			return nil, errors.New("Git plugins require --revision with a full immutable commit hash")
		}
		if e = run(stage, "git", "-c", "protocol.file.allow=never", "clone", "--no-checkout", "--filter=blob:none", remote, "source"); e != nil {
			return nil, e
		}
		target = filepath.Join(stage, "source")
		if e = run(target, "git", "-c", "protocol.file.allow=never", "fetch", "--depth=1", "origin", o.Revision); e != nil {
			return nil, e
		}
		if e = run(target, "git", "checkout", "--detach", o.Revision); e != nil {
			return nil, e
		}
		revision = strings.ToLower(o.Revision)
		source = remote
		os.RemoveAll(filepath.Join(target, ".git"))
	default:
		absolute, e := filepath.Abs(source)
		if e != nil {
			return nil, e
		}
		info, e := os.Lstat(absolute)
		if e != nil {
			return nil, e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("resolve the explicit local plugin path before installing")
		}
		source = absolute
		if info.IsDir() {
			if e = copyTree(absolute, stage); e != nil {
				return nil, e
			}
		} else {
			if !info.Mode().IsRegular() {
				return nil, errors.New("local plugin is not a regular file")
			}
			ext := strings.ToLower(filepath.Ext(absolute))
			switch ext {
			case ".so", ".dll", ".dylib":
				kind = "native"
			case ".js", ".mjs", ".cjs", ".ts", ".tsx":
				kind = "community"
			default:
				return nil, errors.New("unsupported local plugin file extension")
			}
			target = filepath.Join(stage, filepath.Base(absolute))
			b, e := readLimit(absolute, 64<<20)
			if e != nil {
				return nil, e
			}
			if kind == "native" {
				valid := len(b) >= 4 && (string(b[:4]) == "\x7fELF" || string(b[:2]) == "MZ" || b[0] == 0xfe || b[0] == 0xcf || b[0] == 0xce || b[0] == 0xca)
				if !valid {
					return nil, errors.New("native plugin has no supported executable header")
				}
			}
			if e = Atomic(target, b, 0700); e != nil {
				return nil, e
			}
		}
	}
	if kind == "community" && !strings.HasPrefix(source, "npm:") && !strings.HasPrefix(source, "magpie:") {
		if _, e := os.Stat(filepath.Join(target, "package.json")); e == nil {
			args := []string{"bun", "install", "--ignore-scripts", "--no-progress"}
			if _, e := os.Stat(filepath.Join(target, "bun.lock")); e == nil {
				args = append(args, "--frozen-lockfile")
			}
			if e = run(target, args...); e != nil {
				return nil, e
			}
		}
	}
	hash, e := digest(stage)
	if kind == "native" {
		hash, e = fileHash(target)
	}
	if e != nil {
		return nil, e
	}
	if o.SHA256 != "" && !strings.EqualFold(o.SHA256, hash) {
		return nil, errors.New("plugin checksum mismatch")
	}
	if revision == "" {
		revision = hash
	}
	version, gateway := packageInfo(target)
	relative, _ := filepath.Rel(stage, target)
	gatewayRelative := ""
	if gateway != "" {
		gatewayRelative, _ = filepath.Rel(stage, gateway)
	}
	generation := filepath.Join(root, "packages", o.Name, fmt.Sprintf("%s-%d", hash[:16], time.Now().UnixNano()))
	if e = os.Rename(stage, generation); e != nil {
		return nil, e
	}
	target = filepath.Join(generation, relative)
	gateway = ""
	if gatewayRelative != "" {
		gateway = filepath.Join(generation, gatewayRelative)
	}
	entry := Entry{Name: o.Name, Path: target, Kind: kind, Version: version, Source: source, Revision: revision, SHA256: hash, Gateway: gateway}
	if old >= 0 {
		entry.Enabled = r.Plugins[old].Enabled
		entry.Options = r.Plugins[old].Options
		entry.ConfigFile = r.Plugins[old].ConfigFile
		r.Plugins[old] = entry
	} else {
		r.Plugins = append(r.Plugins, entry)
	}
	if e = Save(root, r); e != nil {
		os.RemoveAll(generation)
		return nil, e
	}
	return map[string]any{"ok": true, "plugin": entry.Public()}, nil
}
