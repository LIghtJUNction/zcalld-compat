package claudemaintenance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

const referenceRevision = "1e174f8f9f613a920203a4658d259456fcab5d0d"
const safeSettings = `{"disableDeepLinkRegistration":"disable"}`

// Host runs only explicitly bound native Claude Code profiles. The native CLI
// owns its credential refresh; this adapter never handles OAuth secrets.
type Host struct{ Root string }

func (h Host) selected(key string) (config, account, string, error) {
	c, err := readConfig(h.Root)
	if err != nil {
		return config{}, account{}, "", err
	}
	a, ok := c.Accounts[key]
	if !ok {
		return c, a, "", ErrConfiguration
	}
	if err = validateAccount(a); err != nil {
		return c, a, "", err
	}
	b, _ := json.Marshal(c)
	return c, a, digest(b), nil
}

func (h Host) assertCurrent(key, expected string) error {
	_, _, current, err := h.selected(key)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrConfiguration
	}
	return nil
}

func (h Host) work() (string, error) {
	base := Store(h.Root).Directory
	if !privateDirectory(base) {
		return "", ErrConfiguration
	}
	dir := filepath.Join(base, "work")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", ErrConfiguration
	}
	if !noLinks(dir) {
		return "", ErrConfiguration
	}
	if err := privatefile.Protect(dir, 0700); err != nil {
		return "", ErrConfiguration
	}
	if !privateDirectory(dir) {
		return "", ErrConfiguration
	}
	return dir, nil
}

func capabilities(ctx context.Context, c config, env []string, work string) error {
	if c.Print {
		return nil
	}
	helper, err := executable(c.Helper)
	if err != nil || helper != c.Helper {
		return ErrConfiguration
	}
	capctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, _, err := runProcess(capctx, helper, []string{"--capabilities"}, env, work, nil, false, nil)
	if err != nil {
		return errors.New("Claude interactive helper is unavailable")
	}
	var caps struct {
		Version     int    `json:"schema_version"`
		Interactive bool   `json:"interactive"`
		Binary      bool   `json:"claude_binary_absolute"`
		Config      bool   `json:"config_dir_explicit"`
		Profile     bool   `json:"profile_path_explicit"`
		Stream      bool   `json:"stream_json"`
		Fallback    bool   `json:"print_fallback"`
		WSL         bool   `json:"wsl"`
		Source      string `json:"source_revision"`
	}
	if json.Unmarshal(out, &caps) != nil || caps.Version != 1 || !caps.Interactive || !caps.Binary || !caps.Config || !caps.Profile || !caps.Stream || caps.Fallback || caps.WSL || caps.Source != referenceRevision {
		return errors.New("Claude interactive helper capability is incompatible")
	}
	return nil
}

func usageArgs() []string {
	return []string{"-p", "/usage", "--output-format", "json", "--verbose", "--tools", "", "--strict-mcp-config", "--setting-sources", "", "--settings", safeSettings, "--no-session-persistence"}
}
func warmArgs(stream bool) []string {
	args := []string{"-p", "--output-format", "json", "--model", "haiku", "--tools", "", "--strict-mcp-config", "--setting-sources", "", "--settings", safeSettings}
	if !stream {
		return append(args, "--no-session-persistence")
	}
	args[2] = "stream-json"
	return append(args, "--input-format", "stream-json", "--verbose")
}

func (h Host) inspectLocked(ctx context.Context, key string, c config, a account, stamp, work string) (maintenance.Snapshot, error) {
	result := maintenance.Snapshot{Account: key, Windows: []maintenance.Window{}, Cards: []maintenance.ResetCard{}}
	if err := h.assertCurrent(key, stamp); err != nil {
		return result, err
	}
	binary, err := executable(c.Binary)
	if err != nil || binary != c.Binary {
		return result, ErrConfiguration
	}
	env, err := environment(a, true)
	if err != nil {
		return result, err
	}
	out, _, err := runProcess(ctx, binary, usageArgs(), env, work, nil, false, func() error { return h.assertCurrent(key, stamp) })
	if err != nil {
		return result, err
	}
	if err = h.assertCurrent(key, stamp); err != nil {
		return result, err
	}
	now := time.Now()
	u, err := parseUsage(out, now)
	if err != nil {
		return result, err
	}
	held, err := u.held(a, now)
	if err != nil {
		return result, err
	}
	if err = capabilities(ctx, c, env, work); err != nil {
		return result, err
	}
	// NoCredits is the scheduler's generic stop-before-send bit. For this
	// adapter it represents Claude's no-extra/extra-cap rule, never Codex cards.
	result.Enabled = true
	result.Fresh = true
	result.ObservedAt = now
	result.UsageKnown = u.Known
	result.ModelsKnown = true
	result.NoCredits = true
	result.Held = held
	result.Windows = u.Windows
	return result, nil
}

func (h Host) Inspect(ctx context.Context, key string) (maintenance.Snapshot, error) {
	c, a, stamp, err := h.selected(key)
	if err != nil {
		return maintenance.Snapshot{}, err
	}
	release, err := markRun(a)
	if err != nil {
		return maintenance.Snapshot{}, err
	}
	defer release()
	work, err := h.work()
	if err != nil {
		return maintenance.Snapshot{}, err
	}
	return h.inspectLocked(ctx, key, c, a, stamp, work)
}

func completed(b []byte, stream bool) bool {
	var messages []json.RawMessage
	if stream {
		s := bufio.NewScanner(bytes.NewReader(b))
		s.Buffer(make([]byte, 4096), maxInput)
		for s.Scan() {
			if len(bytes.TrimSpace(s.Bytes())) > 0 {
				messages = append(messages, append(json.RawMessage(nil), s.Bytes()...))
			}
		}
		if s.Err() != nil {
			return false
		}
	} else if json.Unmarshal(b, &messages) != nil {
		messages = []json.RawMessage{b}
	}
	results := 0
	for _, raw := range messages {
		var m struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		if m.Type == "result" {
			if m.IsError {
				return false
			}
			results++
		}
	}
	return results == 1
}

func (h Host) Warmup(ctx context.Context, key string, windows []string) (maintenance.ActionResult, error) {
	result, err := h.warmup(ctx, key, windows)
	if result.Status == "not_sent" && err != nil {
		result.Code = "preflight_failed"
		return result, nil
	}
	return result, err
}

func (h Host) warmup(ctx context.Context, key string, windows []string) (maintenance.ActionResult, error) {
	no := maintenance.ActionResult{Status: "not_sent"}
	c, a, stamp, err := h.selected(key)
	if err != nil {
		return no, err
	}
	release, err := markRun(a)
	if err != nil {
		return no, err
	}
	defer release()
	work, err := h.work()
	if err != nil {
		return no, err
	}
	// An explicit/manual action also honors the selected account's current cap.
	// The fresh read is through the non-generating print /usage path.
	s, err := h.inspectLocked(ctx, key, c, a, stamp, work)
	if err != nil {
		return no, err
	}
	if s.Held {
		return maintenance.ActionResult{Status: "not_sent", Code: "usage_held"}, nil
	}
	if len(windows) == 0 || len(windows) > 64 {
		return no, ErrConfiguration
	}
	seen := map[string]bool{}
	for _, name := range windows {
		if seen[name] {
			return no, ErrConfiguration
		}
		seen[name] = true
		ok := false
		for _, w := range s.Windows {
			if w.Name == name && !w.Aside && w.Model == "" && w.SpanSeconds > 0 {
				ok = true
			}
		}
		if !ok {
			return no, ErrConfiguration
		}
	}
	env, err := environment(a, false)
	if err != nil {
		return no, err
	}
	binary := c.Binary
	args := warmArgs(false)
	input := []byte("hi\n")
	if !c.Print {
		deadline, ok := ctx.Deadline()
		if !ok {
			return no, errors.New("Claude warmup requires a deadline")
		}
		binary = c.Helper
		// Its 3.5s PTY drain must finish before the outer process's -4s
		// cancellation boundary. This reserves time from the same budget.
		args = []string{"--claude-binary", c.Binary, "--config-dir", a.ConfigDir, "--profile", a.Profile, "--work-dir", work, "--deadline-ms", strconv.FormatInt(deadline.Add(-8*time.Second).UnixMilli(), 10)}
		if c.TrustWorkspace {
			args = append(args, "--trust-workspace")
		}
		args = append(args, "--")
		args = append(args, warmArgs(true)...)
		input = []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n")
	}
	out, started, err := runProcess(ctx, binary, args, env, work, input, !c.Print, func() error { return h.assertCurrent(key, stamp) })
	if !started {
		return no, err
	}
	if err != nil || !completed(out, !c.Print) {
		return maintenance.ActionResult{Status: "unknown", Code: "outcome_unknown"}, err
	}
	if err = h.assertCurrent(key, stamp); err != nil {
		return maintenance.ActionResult{Status: "unknown", Code: "identity_changed"}, err
	}
	return maintenance.ActionResult{Status: "completed", Code: "warmed"}, nil
}

func (Host) Consume(context.Context, string, maintenance.ConsumeRequest) (maintenance.ActionResult, error) {
	return maintenance.ActionResult{Status: "not_sent", Code: "unsupported"}, errors.New("Claude has no reset-card maintenance")
}

func Run(ctx context.Context, root string) (maintenance.Report, error) {
	return (maintenance.Engine{Store: Store(root), Host: Host{Root: root}}).Run(ctx)
}

func Manual(ctx context.Context, root, key string, all bool) (maintenance.Report, error) {
	c, err := readConfig(root)
	if err != nil {
		return maintenance.Report{}, err
	}
	keys := []string{}
	if all {
		for k, a := range c.Accounts {
			if a.Enabled && (a.Automatic || a.NativeBinding) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
	} else {
		if !c.Accounts[key].Enabled {
			return maintenance.Report{}, ErrConfiguration
		}
		keys = []string{key}
	}
	if len(keys) == 0 {
		return maintenance.Report{Status: "complete", Results: []maintenance.AccountResult{}}, nil
	}
	engine := maintenance.Engine{Store: Store(root), Host: Host{Root: root}, ManualHost: func(string) (maintenance.Host, func()) { return Host{Root: root}, func() {} }}
	return engine.WarmAccounts(ctx, keys, all)
}
