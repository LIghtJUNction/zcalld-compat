package claudetui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// nativeProbe only runs in the compiled synthetic Claude test executable.
// The native acceptance script supplies temporary files and no credentials.
func nativeProbe() {
	if os.Getenv("ZCALLD_TUI_NATIVE_PROBE") != "1" {
		return
	}
	if os.Getenv("ZCALLD_TUI_NATIVE_IGNORE_STOP") == "1" {
		nativeIgnoreStop()
	}
	explicit := os.Getenv("CLAUDE_CONFIG_DIR")
	cfg := explicit
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".claude")
	}
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		os.Exit(90)
	}
	profile := filepath.Join(cfg, ".claude.json")
	if explicit == "" {
		profile = filepath.Join(filepath.Dir(cfg), ".claude.json")
	}
	b, err := json.Marshal(map[string]any{
		"pid": os.Getpid(), "args": os.Args[1:], "config_env": explicit,
		"config": cfg, "profile": profile, "home": os.Getenv("HOME"),
		"userprofile": os.Getenv("USERPROFILE"), "bypass": os.Getenv(BypassEnv),
		"guard": os.Getenv(GuardEnv), "helper_config": os.Getenv(ConfigDirEnv),
	})
	if err != nil || os.WriteFile(filepath.Join(cfg, "native-launch.json"), b, 0o600) != nil {
		os.Exit(91)
	}
	// The upstream fake reads only this explicit variable. Record its original
	// absence first so the native-layout assertion cannot be masked by this shim.
	_ = os.Setenv("CLAUDE_CONFIG_DIR", cfg)
}
