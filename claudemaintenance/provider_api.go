package claudemaintenance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

// ProviderLease uses the exact native binding and account handover lease used
// by maintenance. It carries no token and never imports or refreshes one.
// The provider is interactive even if maintenance explicitly uses print mode.
type ProviderLease struct {
	host              Host
	key, stamp, model string
	config            config
	account           account
	directory         string
	release           func()
	once              sync.Once
	mu                sync.Mutex
	uncertain, closed bool
}

var ErrProviderHeld = errors.New("Claude account usage limit is reached")
var ErrProviderDrain = errors.New("Claude native process shutdown is unconfirmed")

// NativeProviderConfiguration is a read-only configuration generator for an
// explicitly installed native Community plugin. Existing ordinary Community
// bindings are never converted, replaced, disabled or re-registered here.
// Registration uses the existing checksum-verified install/config/enable CLI.
func NativeProviderConfiguration(root string) (map[string]any, error) {
	if !filepath.IsAbs(root) || !privateDirectory(root) {
		return nil, ErrConfiguration
	}
	if _, err := os.Lstat(filepath.Join(root, "host.json")); !os.IsNotExist(err) {
		return nil, errors.New("an existing community host must keep its ordinary configuration; the native Claude provider is already dispatched by that bridge")
	}
	registryPath := filepath.Join(root, "registry.json")
	_, presentErr := os.Lstat(registryPath)
	if presentErr == nil {
		registry, err := readPrivate(registryPath)
		if err != nil {
			return nil, ErrConfiguration
		}
		var r struct {
			Version int `json:"version"`
			Plugins []struct {
				Kind string `json:"kind"`
			} `json:"plugins"`
		}
		if json.Unmarshal(registry, &r) != nil || r.Version != 1 {
			return nil, ErrConfiguration
		}
		for _, p := range r.Plugins {
			if p.Kind == "community" {
				return nil, errors.New("existing community plugins must retain their ordinary binding")
			}
		}
	} else if !os.IsNotExist(presentErr) {
		return nil, ErrConfiguration
	}
	doc, err := Store(root).Read()
	if err != nil {
		return nil, err
	}
	var c config
	if decode(doc.Adapter, &c) != nil || c.Version != 1 || len(c.Accounts) == 0 {
		return nil, errors.New("bind an explicit native Claude profile first")
	}
	available := false
	for key, a := range c.Accounts {
		if a.Enabled {
			if _, _, _, err = (Host{Root: root}).selected(key); err != nil {
				return nil, err
			}
			available = true
		}
	}
	if !available {
		return nil, errors.New("no enabled native Claude profile is bound")
	}
	helper, err := os.Executable()
	if err != nil {
		return nil, ErrConfiguration
	}
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		return nil, ErrConfiguration
	}
	helper, err = executable(helper)
	if err != nil {
		return nil, ErrConfiguration
	}
	return map[string]any{"claude_only": true, "root": root, "pluginctl": helper}, nil
}

func (h Host) OpenProvider(ctx context.Context, key, model string) (*ProviderLease, error) {
	c, a, stamp, err := h.selected(key)
	if err != nil {
		return nil, err
	}
	if c.Helper == "" {
		return nil, errors.New("Claude interactive helper is required")
	}
	c.Print = false
	unlock, err := markRun(a)
	if err != nil {
		return nil, err
	}
	l := &ProviderLease{host: h, key: key, stamp: stamp, model: model, config: c, account: a, release: unlock}
	failed := true
	defer func() {
		if failed {
			l.Close()
		}
	}()
	base, err := h.work()
	if err != nil {
		return nil, err
	}
	l.directory, err = os.MkdirTemp(base, "provider-")
	if err != nil {
		return nil, ErrConfiguration
	}
	if err = privatefile.Protect(l.directory, 0700); err != nil || !privateDirectory(l.directory) {
		return nil, ErrConfiguration
	}
	env, err := environment(a, false)
	if err != nil {
		return nil, err
	}
	if err = providerCapabilities(ctx, c, env, l.directory); err != nil {
		if errors.Is(err, errProcessDrain) {
			l.uncertain = true
			failed = false
			return l, ErrProviderDrain
		}
		return nil, err
	}
	if err = l.Check(ctx); err != nil {
		if errors.Is(err, ErrProviderDrain) {
			failed = false
			return l, err
		}
		return nil, err
	}
	failed = false
	return l, nil
}

func (l *ProviderLease) Directory() string { return l.directory }
func (l *ProviderLease) Helper() string    { return l.config.Helper }

// Check runs before the native CLI starts and before a resumed tool result can
// let it make another request. Ready rechecks identity without a second query.
// Unrestricted accounts do not get an additional quota query. A restricted account uses
// the existing fixed -p /usage command, never an interactive quota probe.
func (l *ProviderLease) Check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.uncertain {
		return ErrProviderDrain
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.host.assertCurrent(l.key, l.stamp); err != nil {
		return err
	}
	a := l.account
	if !a.NoExtra && a.ExtraShare <= 0 && a.ExtraAmount <= 0 {
		return nil
	}
	env, err := environment(a, true)
	if err != nil {
		return err
	}
	binary, err := executable(l.config.Binary)
	if err != nil || binary != l.config.Binary {
		return ErrConfiguration
	}
	query, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := runProcess(query, binary, usageArgs(), env, l.directory, nil, false, func() error { return l.host.assertCurrent(l.key, l.stamp) })
	if err != nil {
		if errors.Is(err, errProcessDrain) {
			l.uncertain = true
			return ErrProviderDrain
		}
		return err
	}
	if err = l.host.assertCurrent(l.key, l.stamp); err != nil {
		return err
	}
	u, err := parseUsage(out, time.Now())
	if err != nil {
		return err
	}
	held, err := u.heldForModel(a, l.model, time.Now())
	if err != nil {
		return err
	}
	if held {
		return ErrProviderHeld
	}
	return nil
}

func (l *ProviderLease) Current() error { return l.host.assertCurrent(l.key, l.stamp) }

// Write owns every provider-side temporary file; no request supplies a path.
func (l *ProviderLease) Write(name string, data []byte) (string, error) {
	if filepath.Base(name) != name || name == "." || len(data) > 8<<20 {
		return "", ErrConfiguration
	}
	path := filepath.Join(l.directory, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", ErrConfiguration
	}
	defer f.Close()
	if err = privatefile.Protect(path, 0600); err != nil {
		return "", err
	}
	if _, err = f.Write(data); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

// Close is called only after the owned process finalizer confirms termination.
// A provider retains the lease on an unconfirmed drain instead of permitting
// another account owner to reuse a potentially active native directory.
func (l *ProviderLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uncertain {
		return ErrProviderDrain
	}
	l.once.Do(func() {
		l.closed = true
		if l.directory != "" {
			_ = os.RemoveAll(l.directory)
		}
		if l.release != nil {
			l.release()
		}
	})
	return nil
}

type ProviderLaunch struct {
	MCPConfig  string
	SystemFile string
	MaxTokens  int
	Effort     string
}

type ProviderProcess struct {
	Output   io.ReadCloser
	input    io.WriteCloser
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	drainErr error
	deadline time.Time
	finish   func(time.Time) error
	write    sync.Mutex
}

func (l *ProviderLease) Start(ctx context.Context, spec ProviderLaunch) (*ProviderProcess, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !time.Now().Before(deadline.Add(-8*time.Second)) {
		return nil, context.DeadlineExceeded
	}
	if err := l.Current(); err != nil {
		return nil, err
	}
	helper, err := executable(l.config.Helper)
	if err != nil || helper != l.config.Helper {
		return nil, ErrConfiguration
	}
	binary, err := executable(l.config.Binary)
	if err != nil || binary != l.config.Binary {
		return nil, ErrConfiguration
	}
	env, err := environment(l.account, false)
	if err != nil {
		return nil, err
	}
	// These are validated request controls, never arbitrary environment input.
	env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(spec.MaxTokens), "CLAUDE_CODE_MAX_RETRIES=0", "MCP_CONNECTION_NONBLOCKING=0")
	args := []string{"--claude-binary", binary, "--config-dir", l.account.ConfigDir, "--profile", l.account.Profile, "--work-dir", l.directory, "--deadline-ms", strconv.FormatInt(deadline.Add(-8*time.Second).UnixMilli(), 10)}
	if l.config.TrustWorkspace {
		args = append(args, "--trust-workspace")
	}
	args = append(args, "--", "--output-format", "stream-json", "--input-format", "stream-json", "--include-partial-messages", "--verbose", "--model", l.model, "--tools", "", "--strict-mcp-config", "--mcp-config", spec.MCPConfig, "--setting-sources", "", "--settings", safeSettings)
	if spec.SystemFile != "" {
		args = append(args, "--append-system-prompt-file", spec.SystemFile)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	call, cancel := context.WithDeadline(ctx, deadline.Add(-4*time.Second))
	cmd := exec.CommandContext(call, helper, args...)
	cmd.Dir = l.directory
	cmd.Env = env
	cmd.WaitDelay = 3500 * time.Millisecond
	p := &ProviderProcess{cmd: cmd, cancel: cancel, done: make(chan struct{}), deadline: deadline}
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	p.input = input
	output, writer := io.Pipe()
	p.Output = output
	cmd.Stdout = writer
	cmd.Stderr = &limitedOutput{limit: 32 << 10}
	after, stop, finish, err := prepareProcess(cmd)
	if err != nil {
		cancel()
		input.Close()
		output.Close()
		writer.Close()
		return nil, err
	}
	p.finish = finish
	cmd.Cancel = func() error {
		_ = input.Close()
		if runtime.GOOS == "windows" {
			return nil
		}
		return stop()
	}
	if err = cmd.Start(); err != nil {
		cancel()
		input.Close()
		output.Close()
		writer.Close()
		p.drainErr = finish(deadline)
		p.err = errors.New("Claude provider process could not start")
		close(p.done)
		if p.drainErr != nil {
			return p, ErrProviderDrain
		}
		return nil, p.err
	}
	if err = after(call); err != nil {
		_ = cmd.Process.Kill()
		output.Close()
		_ = cmd.Wait()
		writer.Close()
		cancel()
		input.Close()
		p.err = errors.New("Claude provider process ownership is unavailable")
		p.drainErr = finish(deadline)
		close(p.done)
		return p, p.err
	}
	go func() {
		p.err = cmd.Wait()
		_ = writer.Close()
		p.drainErr = finish(earlier(deadline, time.Now().Add(4*time.Second)))
		if p.drainErr != nil {
			p.err = p.drainErr
		}
		close(p.done)
	}()
	return p, nil
}

func (p *ProviderProcess) Send(message []byte) error {
	p.write.Lock()
	defer p.write.Unlock()
	select {
	case <-p.done:
		return errors.New("Claude provider process ended")
	default:
	}
	_, err := p.input.Write(append(append([]byte(nil), message...), '\n'))
	return err
}

func (p *ProviderProcess) Done() <-chan struct{} { return p.done }
func (p *ProviderProcess) Result() error         { <-p.done; return p.err }
func (p *ProviderProcess) Close() error {
	_ = p.input.Close()
	p.cancel()
	_ = p.Output.Close()
	remaining := time.Until(earlier(p.deadline, time.Now().Add(4*time.Second)))
	if remaining <= 0 {
		select {
		case <-p.done:
			return p.drainErr
		default:
			return errProcessDrain
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.drainErr
	case <-timer.C:
		return errProcessDrain
	}
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
