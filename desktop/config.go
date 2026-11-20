package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const MaxConfigBytes = 16 << 10

var ErrConfig = errors.New("desktop notification settings are unavailable")

// Config is the host-owned desktop.json schema. Reading it never creates a
// directory, launches a consumer, changes permissions or performs a network call.
type Config struct {
	Enabled            bool    `json:"enabled"`
	KeepAwake          bool    `json:"keep_awake"`
	KeepDisplay        bool    `json:"keep_display"`
	UsageAlert         uint32  `json:"usage_alert"`
	BalanceAlert       float64 `json:"balance_alert"`
	ResetReminderHours uint32  `json:"reset_reminder_hours"`
	TurnDoneSeconds    uint32  `json:"turn_done_seconds"`
	MaskAlert          bool    `json:"mask_alert"`
	Language           string  `json:"language"`
	StateDir           string  `json:"state_dir"`
	Inbox              string  `json:"inbox"`
	Backend            string  `json:"backend"`
	NotificationHelper string  `json:"notification_helper"`
}

func (c Config) QuotaEnabled() bool {
	return c.Enabled && (c.UsageAlert > 0 || c.BalanceAlert > 0 || c.ResetReminderHours > 0)
}

func configPath(s string) bool {
	return s != "" && len(s) <= 4096 && filepath.IsAbs(s) && filepath.Clean(s) == s &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

func decodeConfig(data []byte, parent string) (Config, error) {
	var config Config
	dec := json.NewDecoder(bytes.NewReader(data))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return config, ErrConfig
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return config, ErrConfig
		}
		if _, duplicate := fields[key]; duplicate {
			return config, ErrConfig
		}
		switch key {
		case "enabled", "keep_awake", "keep_display", "usage_alert", "balance_alert",
			"reset_reminder_hours", "turn_done_seconds", "mask_alert", "language", "state_dir",
			"inbox", "backend", "notification_helper":
		default:
			return config, ErrConfig
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return config, ErrConfig
		}
		fields[key] = raw
	}
	if close, err := dec.Token(); err != nil || close != json.Delim('}') {
		return config, ErrConfig
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return config, ErrConfig
	}
	if raw, exists := fields["enabled"]; exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return config, ErrConfig
	}
	// The typed decode rejects string booleans, fractional integer thresholds,
	// overflows and non-finite JSON numbers. No unknown field is retained.
	if json.Unmarshal(data, &config) != nil {
		return Config{}, ErrConfig
	}
	if config.UsageAlert > 100 || config.ResetReminderHours > 24*366 || config.TurnDoneSeconds > 86400 ||
		math.IsNaN(config.BalanceAlert) || math.IsInf(config.BalanceAlert, 0) || config.BalanceAlert < 0 {
		return Config{}, ErrConfig
	}
	switch config.Language {
	case "":
		config.Language = "en"
	case "en", "zh", "zh-CN", "zh-TW", "ja", "de":
	default:
		return Config{}, ErrConfig
	}
	switch config.Backend {
	case "":
		config.Backend = "native"
	case "native", "record":
	default:
		return Config{}, ErrConfig
	}
	if _, exists := fields["inbox"]; !exists {
		config.Inbox = filepath.Join(parent, "desktop", "inbox")
	}
	if _, exists := fields["state_dir"]; !exists {
		config.StateDir = filepath.Join(parent, "desktop", "state")
	}
	if raw, exists := fields["notification_helper"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !configPath(config.NotificationHelper) {
		return Config{}, ErrConfig
	}
	return config, nil
}

// ReadConfig resolves the exact sibling ../desktop.json of an extensions root.
// Missing configuration is disabled. An enabled quota producer uses only the
// explicit options and the private, pre-existing consumer inbox.
func ReadConfig(extensionsRoot string) (Config, error) {
	if !configPath(extensionsRoot) {
		return Config{}, ErrConfig
	}
	parent := filepath.Dir(extensionsRoot)
	path := filepath.Join(parent, "desktop.json")
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > MaxConfigBytes ||
		!privatePath(path, before) {
		return Config{}, ErrConfig
	}
	dir, err := os.Lstat(parent)
	if err != nil || !dir.IsDir() || !privatePath(parent, dir) {
		return Config{}, ErrConfig
	}
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil || canonicalParent != parent {
		return Config{}, ErrConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, ErrConfig
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) {
		return Config{}, ErrConfig
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	after, statErr := os.Lstat(path)
	if err != nil || len(raw) > MaxConfigBytes || int64(len(raw)) != before.Size() ||
		statErr != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !privatePath(path, after) {
		return Config{}, ErrConfig
	}
	config, err := decodeConfig(raw, parent)
	if err != nil {
		return Config{}, err
	}
	if !configPath(config.Inbox) || !configPath(config.StateDir) || config.Inbox == config.StateDir ||
		(config.NotificationHelper != "" && !configPath(config.NotificationHelper)) {
		return Config{}, ErrConfig
	}
	if !config.QuotaEnabled() {
		return config, nil
	}
	info, err := os.Lstat(config.Inbox)
	if err != nil || !info.IsDir() || !privatePath(config.Inbox, info) {
		return Config{}, ErrConfig
	}
	canonical, err := filepath.EvalSymlinks(config.Inbox)
	if err != nil || canonical != config.Inbox {
		return Config{}, ErrConfig
	}
	return config, nil
}
