package claudemaintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

func privateDirectory(path string) bool { return noLinks(path) && privatefile.CheckDirectory(path) }
func executable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrConfiguration
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrConfiguration
	}
	i, err := os.Stat(resolved)
	if err != nil || !i.Mode().IsRegular() || (runtime.GOOS != "windows" && i.Mode().Perm()&0111 == 0) || (runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(resolved), ".exe")) {
		return "", ErrConfiguration
	}
	return resolved, nil
}
func accountHome(a account) (string, error) {
	if a.Profile == filepath.Join(a.ConfigDir, ".claude.json") {
		return "", nil
	}
	if filepath.Base(a.Profile) == ".claude.json" && a.ConfigDir == filepath.Join(filepath.Dir(a.Profile), ".claude") {
		return filepath.Dir(a.Profile), nil
	}
	return "", errors.New("Claude profile/config layout is unsupported")
}

func proxyEnvironment(proxy string) ([]string, error) {
	if proxy == "" {
		return nil, nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, ErrConfiguration
	}
	return []string{"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "ALL_PROXY=" + proxy}, nil
}

func environment(a account, usageRead bool) ([]string, error) {
	home, err := accountHome(a)
	if err != nil {
		return nil, err
	}
	env := []string{}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "CLAUDE_") || strings.HasPrefix(upper, "MAGPIE_") || strings.HasPrefix(upper, "ZCALLD_CLAUDE_") || upper == "NODE_OPTIONS" || upper == "BUN_OPTIONS" || upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY" || upper == "CLAUDECODE" {
			continue
		}
		if home != "" && (upper == "HOME" || upper == "USERPROFILE") {
			continue
		}
		env = append(env, value)
	}
	if home == "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	} else {
		env = append(env, "HOME="+home)
		if runtime.GOOS == "windows" {
			env = append(env, "USERPROFILE="+home)
		}
	}
	env = append(env, "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "DISABLE_AUTOUPDATER=1")
	// /usage needs its own non-generating endpoint fetch. Interactive generation
	// suppresses startup background traffic; neither path inherits caller URLs.
	if !usageRead {
		env = append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}
	proxy, err := proxyEnvironment(a.Proxy)
	if err != nil {
		return nil, err
	}
	return append(env, proxy...), nil
}

// Keep the buffer named: promoting bytes.Buffer.ReadFrom would let io.Copy
// bypass Write, including both its limit and resultOutput's handshake hooks.
type limitedOutput struct {
	buf   bytes.Buffer
	limit int
}

func (w *limitedOutput) Len() int      { return w.buf.Len() }
func (w *limitedOutput) Bytes() []byte { return w.buf.Bytes() }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.Len() {
		return 0, errors.New("Claude output exceeded its limit")
	}
	return w.buf.Write(p)
}

type resultOutput struct {
	limitedOutput
	line            []byte
	done, ready     func()
	once, readyOnce sync.Once
}

var errProcessDrain = errors.New("Claude process exit could not be confirmed")

func (w *resultOutput) Write(p []byte) (int, error) {
	n, err := w.limitedOutput.Write(p)
	if err != nil {
		return n, err
	}
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		var m struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(w.line[:i], &m) == nil {
			if m.Type == "result" {
				w.once.Do(w.done)
			}
			if m.Type == "system" && m.Subtype == "init" {
				w.readyOnce.Do(w.ready)
			}
		}
		w.line = w.line[i+1:]
	}
	return n, nil
}

// runProcess keeps all execution and cancellation within the caller's original
// deadline. OS preparation establishes ownership before allowing Windows code
// to run. Returned started means generation may have happened, not success.
func runProcess(ctx context.Context, binary string, args, env []string, work string, input []byte, interactive bool, before func() error) (out []byte, started bool, err error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, false, errors.New("Claude operation requires a deadline")
	}
	operation := deadline.Add(-4 * time.Second)
	if !time.Now().Before(operation) {
		return nil, false, context.DeadlineExceeded
	}
	call, cancel := context.WithDeadline(ctx, operation)
	defer cancel()
	if before != nil {
		if err = before(); err != nil {
			return nil, false, err
		}
	}
	cmd := exec.CommandContext(call, binary, args...)
	cmd.Dir = work
	cmd.Env = env
	stdout := &resultOutput{limitedOutput: limitedOutput{limit: maxInput}, done: func() {}, ready: func() {}}
	stderr := &limitedOutput{limit: 32 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Leave the original deadline intact. A real interactive helper needs up
	// to 3s to close its PTY, then 500ms to confirm the killed child exited.
	cmd.WaitDelay = 3500 * time.Millisecond
	var writeInput func()
	closeInput := func() {}
	if interactive {
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			return nil, false, pipeErr
		}
		closeInput = func() { _ = stdin.Close() }
		stdout.done = closeInput
		writeInput = func() { _, _ = stdin.Write(input) }
	} else {
		cmd.Stdin = bytes.NewReader(input)
	}
	if interactive {
		stdout.ready = func() {
			if before != nil {
				if e := before(); e != nil {
					closeInput()
					cancel()
					return
				}
			}
			if call.Err() != nil {
				closeInput()
				return
			}
			go writeInput()
		}
	}
	defer closeInput()
	startedChild, stop, cleanup, prepErr := prepareProcess(cmd)
	if prepErr != nil {
		return nil, false, prepErr
	}
	defer func() {
		if finishErr := cleanup(deadline); finishErr != nil {
			out = nil
			err = finishErr
		}
	}()
	cmd.Cancel = func() error {
		closeInput()
		// ConPTY has no Unix TERM signal. EOF lets the helper finish its own
		// bounded drain; WaitDelay and the owned job are the final fallback.
		if interactive && runtime.GOOS == "windows" {
			return nil
		}
		return stop()
	}
	if err = cmd.Start(); err != nil {
		return nil, false, errors.New("Claude process could not start")
	}
	if err = startedChild(call); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, false, err
	}
	started = true
	err = cmd.Wait()
	if err != nil {
		return nil, true, errors.New("Claude process did not complete")
	}
	return stdout.Bytes(), true, nil
}
