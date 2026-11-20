package claudetui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
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
	if os.Getenv("ZCALLD_TUI_NATIVE_LAUNCH_ACK") == "1" {
		// The Windows sign-in fixture retains a process handle before allowing
		// the screen that makes the helper stop this child. Use the existing
		// ten-second launch bound; this opt-in lives only in the test executable.
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		want := strconv.Itoa(os.Getpid()) + "\n"
		for {
			ack, err := os.ReadFile(filepath.Join(cfg, "native-launch.ack"))
			if err == nil && string(ack) == want {
				break
			}
			if err != nil && !os.IsNotExist(err) {
				os.Exit(92)
			}
			select {
			case <-deadline.C:
				os.Exit(93)
			case <-tick.C:
			}
		}
	}
	// The upstream fake reads only this explicit variable. Record its original
	// absence first so the native-layout assertion cannot be masked by this shim.
	_ = os.Setenv("CLAUDE_CONFIG_DIR", cfg)
}
