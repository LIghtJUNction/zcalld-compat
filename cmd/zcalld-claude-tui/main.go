// zcalld-claude-tui is an explicit, one-shot launcher for the pinned Magpie
// interactive PTY bridge. Account selection and policy remain in its caller.
package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/buildinfo"
	"github.com/LIghtJUNction/zcalld-compat/internal/claudebridge"
	"github.com/LIghtJUNction/zcalld-compat/internal/claudetui"
)

const sourceRevision = "1e174f8f9f613a920203a4658d259456fcab5d0d"

type launch struct {
	binary, config, profile, work string
	deadline                      int64
	trust                         bool
	args                          []string
	nativeHome                    string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "zcalld-claude-tui:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == claudetui.MCPCommand {
		return claudebridge.RunMCP(args[1:], os.Stdin, out)
	}
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(out, buildinfo.String("zcalld-claude-tui"))
		return err
	}
	// Capability discovery has no filesystem, child process, or auth access.
	if len(args) == 1 && args[0] == "--capabilities" {
		return json.NewEncoder(out).Encode(map[string]any{
			"schema_version": 1, "source_revision": sourceRevision,
			"interactive": true, "stream_json": true, "print_fallback": false,
			"claude_binary_absolute": true, "config_dir_explicit": true,
			"profile_path_explicit": true, "workspace_explicit": true,
			"deadline_required": true, "wsl": false, "conpty": "system",
			"mcp_tool_bridge": true, "owned_drain": true,
			"platform": runtime.GOOS,
		})
	}
	l, err := parse(args)
	if err != nil {
		return err
	}
	for _, p := range []struct {
		path, label string
		directory   bool
	}{
		{l.binary, "Claude executable", false}, {l.config, "config directory", true},
		{l.profile, "profile", false}, {l.work, "workspace", true},
	} {
		fi, err := os.Stat(p.path)
		if err != nil || fi.IsDir() != p.directory || !p.directory && !fi.Mode().IsRegular() {
			return fmt.Errorf("the explicit %s is unavailable", p.label)
		}
		if p.path == l.binary && runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
			return errors.New("the explicit Claude executable is not executable")
		}
		if p.path == l.work && l.trust && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			return errors.New("workspace trust requires a private workspace")
		}
	}
	remaining := time.Until(time.UnixMilli(l.deadline))
	if remaining <= 0 || remaining > 5*time.Minute {
		return errors.New("deadline must be in the future and at most five minutes away")
	}
	// Never inherit another helper's account-mode, consent, WSL, or request-body
	// guard. Native Claude remains the owner of credentials in the explicit layout.
	for _, name := range []string{claudetui.TrustEnv, claudetui.BypassEnv,
		claudetui.WSLEnv, claudetui.GuardEnv, claudetui.DeadlineEnv,
		claudetui.ConfigDirEnv, "MAGPIE_CLAUDE_TAP", "CLAUDE_CONFIG_DIR"} {
		if err := os.Unsetenv(name); err != nil {
			return errors.New("could not configure the owned helper environment")
		}
	}
	set := map[string]string{
		claudetui.ConfigDirEnv: l.config,
		claudetui.DeadlineEnv:  fmt.Sprint(l.deadline),
	}
	if l.nativeHome != "" {
		// The ordinary layout has HOME/.claude.json beside HOME/.claude.
		// Setting CLAUDE_CONFIG_DIR here would select a different profile.
		set["HOME"] = l.nativeHome
		if runtime.GOOS == "windows" {
			set["USERPROFILE"] = l.nativeHome
		}
	} else {
		set["CLAUDE_CONFIG_DIR"] = l.config
	}
	if l.trust {
		set[claudetui.TrustEnv] = "1"
	}
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			return errors.New("could not configure the owned helper environment")
		}
	}
	if err := os.Chdir(l.work); err != nil {
		return errors.New("could not enter the explicit workspace")
	}
	// This is the upstream UUID-v4 fallback without Magpie's private keyed
	// session marker. The isolated helper never reads or creates that marker.
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errors.New("could not allocate the helper session identity")
	}
	id[6], id[8] = id[6]&0x0f|0x40, id[8]&0x3f|0x80
	sid := fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:])
	pargs := append([]string{l.binary}, l.args...)
	pargs = append(pargs, "--session-id", sid)
	return claudetui.Run(pargs)
}

func parse(args []string) (launch, error) {
	var l launch
	f := flag.NewFlagSet("zcalld-claude-tui", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&l.binary, "claude-binary", "", "absolute native Claude executable")
	f.StringVar(&l.config, "config-dir", "", "explicit Claude transcript/config directory")
	f.StringVar(&l.profile, "profile", "", "explicit native Claude profile")
	f.StringVar(&l.work, "work-dir", "", "explicit owned workspace")
	f.Int64Var(&l.deadline, "deadline-ms", 0, "absolute Unix-millisecond deadline")
	f.BoolVar(&l.trust, "trust-workspace", false, "accept only this owned workspace trust prompt")
	if err := f.Parse(args); err != nil {
		return l, errors.New("invalid helper arguments")
	}
	for _, p := range []*string{&l.binary, &l.config, &l.profile, &l.work} {
		if !filepath.IsAbs(*p) || strings.ContainsAny(*p, "\x00\r\n") {
			return l, errors.New("binary, config, profile and workspace paths must be explicit and absolute")
		}
		*p = filepath.Clean(*p)
	}
	if strings.EqualFold(filepath.Base(l.binary), "wsl.exe") || strings.EqualFold(filepath.Base(l.binary), "wsl") {
		return l, errors.New("WSL is not enabled in this standalone helper")
	}
	if samePath(l.profile, filepath.Join(l.config, ".claude.json")) {
		// Explicit CLAUDE_CONFIG_DIR layout.
	} else if samePath(l.config, filepath.Join(filepath.Dir(l.profile), ".claude")) && samePath(l.profile, filepath.Join(filepath.Dir(l.profile), ".claude.json")) {
		l.nativeHome = filepath.Dir(l.profile)
	} else {
		return l, errors.New("the explicit config/profile pair is not a supported native Claude layout")
	}
	if l.deadline <= 0 {
		return l, errors.New("an absolute finite deadline is required")
	}
	l.args = f.Args()
	for _, a := range l.args {
		name, _, _ := strings.Cut(a, "=")
		switch name {
		case "--resume", "--continue", "-c", "--session-id":
			return l, errors.New("a maintenance helper cannot resume or select an existing session")
		}
	}
	return l, nil
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
