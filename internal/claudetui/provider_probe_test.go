package claudetui

// This file is linked only into the compiled synthetic Claude executable used
// by test-claude-provider.py. It contains no vendor network or credential code.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

type providerFixture struct {
	Magic          string  `json:"magic"`
	Mode           string  `json:"mode"`
	Used           float64 `json:"used"`
	UsageHold      bool    `json:"usage_hold"`
	SwitchIdentity bool    `json:"switch_identity"`
	dir            string
	mcp            *providerMCP
}

var activeProviderFixture *providerFixture

func providerProbe() bool {
	if len(os.Args) > 2 && os.Args[1] == "--provider-protect-fixture" {
		for _, p := range os.Args[2:] {
			i, e := os.Lstat(p)
			if e != nil || !filepath.IsAbs(p) || i.Mode()&os.ModeSymlink != 0 {
				os.Exit(90)
			}
			mode := os.FileMode(0600)
			if i.IsDir() {
				mode = 0700
			}
			if privatefile.Protect(p, mode) != nil {
				os.Exit(91)
			}
		}
		return true
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	path := filepath.Join(dir, "provider-fixture.json")
	i, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil || !i.Mode().IsRegular() || i.Size() > 4096 {
		os.Exit(92)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		os.Exit(92)
	}
	var f providerFixture
	if json.Unmarshal(b, &f) != nil || f.Magic != "zcalld-native-provider-fixture-v1" {
		os.Exit(92)
	}
	f.dir = dir
	activeProviderFixture = &f
	args := os.Args[1:]
	providerRecord("launch", map[string]any{"pid": os.Getpid(), "args": args, "config_env": os.Getenv("CLAUDE_CONFIG_DIR"), "home": os.Getenv("HOME"), "max_tokens": os.Getenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS"), "secret_env_present": os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_BASE_URL") != "" || os.Getenv("NODE_OPTIONS") != ""})
	if slices.Contains(args, "-p") {
		if len(args) < 2 || args[0] != "-p" || args[1] != "/usage" || !slices.Contains(args, "--verbose") {
			os.Exit(93)
		}
		providerRecord("usage", map[string]any{"pid": os.Getpid()})
		if f.UsageHold {
			nativeIgnoreStop()
			time.Sleep(time.Minute)
			return true
		}
		reset := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
		fmt.Printf(`[{"usage_report":{"rate_limits":{"limits":[{"kind":"weekly_all","percent":%v,"resets_at":%q}],"extra_usage":{"is_enabled":false}}}},{"type":"result","is_error":false,"result":"synthetic usage"}]`, f.Used, reset)
		return true
	}
	if f.Mode == "hold" {
		nativeIgnoreStop()
	}
	_ = os.Setenv("ZCALLD_TUI_NATIVE_PROBE", "1")
	nativeProbe()
	_ = os.Setenv("FAKE_TAP", "1")
	if f.SwitchIdentity {
		profile := filepath.Join(dir, ".claude.json")
		if _, err = os.Stat(profile); os.IsNotExist(err) {
			profile = filepath.Join(filepath.Dir(dir), ".claude.json")
		}
		_ = os.WriteFile(profile, []byte(`{"oauthAccount":{"emailAddress":"changed@example.invalid","organizationUuid":"changed-org","accountUuid":"changed-member"}}`), 0600)
	}
	if mcp, err := newProviderMCP(args); err != nil {
		os.Exit(94)
	} else {
		f.mcp = mcp
		if mcp != nil {
			defer mcp.close()
		}
	}
	fakeClaude(args)
	return true
}

func providerRecord(kind string, value any) {
	f := activeProviderFixture
	if f == nil {
		return
	}
	b, _ := json.Marshal(value)
	p := filepath.Join(f.dir, kind+"-"+strconv.Itoa(os.Getpid())+".json")
	if os.WriteFile(p, b, 0600) != nil {
		os.Exit(95)
	}
}

type providerMCP struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Scanner
	tools []struct {
		Name string `json:"name"`
	}
}

func newProviderMCP(args []string) (*providerMCP, error) {
	i := slices.Index(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) {
		return nil, nil
	}
	var c struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(args[i+1]), &c) != nil {
		return nil, fmt.Errorf("synthetic MCP config")
	}
	v, ok := c.Servers["magpie"]
	if !ok {
		return nil, nil
	}
	if !filepath.IsAbs(v.Command) || len(v.Args) != 4 || v.Args[0] != "claude-mcp-helper" {
		return nil, fmt.Errorf("synthetic MCP command")
	}
	p := &providerMCP{cmd: exec.Command(v.Command, v.Args...)}
	var err error
	p.in, err = p.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p.out = bufio.NewScanner(r)
	p.out.Buffer(make([]byte, 65536), 8<<20)
	p.cmd.Stderr = os.Stderr
	if err = p.cmd.Start(); err != nil {
		return nil, err
	}
	if err = p.send(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "synthetic-claude", "version": "1"}}); err != nil {
		p.close()
		return nil, err
	}
	if _, err = p.read(); err != nil {
		p.close()
		return nil, err
	}
	if err = p.send(2, "tools/list", map[string]any{}); err != nil {
		p.close()
		return nil, err
	}
	result, err := p.read()
	if err != nil {
		p.close()
		return nil, err
	}
	var catalog struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(result, &catalog) != nil || len(catalog.Tools) == 0 {
		p.close()
		return nil, fmt.Errorf("synthetic MCP catalog")
	}
	p.tools = catalog.Tools
	return p, nil
}

func (p *providerMCP) send(id int, method string, params any) error {
	return json.NewEncoder(p.in).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}
func (p *providerMCP) read() (json.RawMessage, error) {
	if !p.out.Scan() {
		return nil, fmt.Errorf("synthetic MCP ended")
	}
	var value struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(p.out.Bytes(), &value) != nil || len(value.Error) > 0 || len(value.Result) == 0 {
		return nil, fmt.Errorf("synthetic MCP rejected")
	}
	return value.Result, nil
}
func (p *providerMCP) close() {
	_ = p.in.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func providerTap(file *os.File, n int, events ...any) {
	for _, event := range events {
		b, _ := json.Marshal(map[string]any{"s": n, "e": event})
		if _, e := file.Write(append(b, '\n')); e != nil {
			os.Exit(96)
		}
	}
}
func providerTapEnd(file *os.File, n int) {
	b, _ := json.Marshal(map[string]any{"s": n, "end": "done"})
	_, _ = file.Write(append(b, '\n'))
	_ = file.Sync()
}
func providerTranscript(dir, sid string, rows ...any) {
	f, e := os.OpenFile(filepath.Join(dir, sid+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(96)
	}
	for _, row := range rows {
		b, _ := json.Marshal(row)
		_, _ = f.Write(append(b, '\n'))
	}
	_ = f.Sync()
	_ = f.Close()
}

func providerTurn(args []string, dir, sid, line, typed string, tap *os.File) bool {
	f := activeProviderFixture
	if f == nil {
		return false
	}
	providerRecord("prompt", map[string]any{"pid": os.Getpid(), "text": typed})
	if f.Mode == "hold" {
		providerTranscript(dir, sid, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": fakeUserRow(typed)}})
		time.Sleep(time.Minute)
		return true
	}
	if f.Mode != "tools" {
		return false
	}
	if tap == nil || f.mcp == nil || len(f.mcp.tools) < 2 {
		os.Exit(97)
	}
	var blocks []any
	providerTap(tap, 1, map[string]any{"type": "message_start", "message": map[string]any{"id": "native_tools", "role": "assistant", "model": "claude-haiku", "usage": map[string]int{"input_tokens": 3}}})
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("native_tool_%d", i)
		name := f.mcp.tools[i].Name
		arguments := map[string]any{"query": fmt.Sprintf("fixture-%d", i)}
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": "mcp__magpie__" + name, "input": arguments})
		encoded, _ := json.Marshal(arguments)
		providerTap(tap, 1,
			map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": id, "name": "mcp__magpie__" + name, "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}},
			map[string]any{"type": "content_block_stop", "index": i})
	}
	providerTap(tap, 1, map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 2}}, map[string]any{"type": "message_stop"})
	providerTapEnd(tap, 1)
	providerTranscript(dir, sid,
		map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": fakeUserRow(typed)}},
		map[string]any{"type": "assistant", "requestId": "request_tools", "message": map[string]any{"id": "native_tools", "role": "assistant", "model": "claude-haiku", "content": blocks, "stop_reason": "tool_use"}})
	for i := 0; i < 2; i++ {
		if f.mcp.send(i+10, "tools/call", map[string]any{"name": f.mcp.tools[i].Name, "arguments": map[string]any{"query": fmt.Sprintf("fixture-%d", i)}, "_meta": map[string]string{"claudecode/toolUseId": fmt.Sprintf("native_tool_%d", i)}}) != nil {
			os.Exit(98)
		}
	}
	results := []json.RawMessage{}
	for i := 0; i < 2; i++ {
		result, err := f.mcp.read()
		if err != nil {
			os.Exit(98)
		}
		results = append(results, result)
	}
	providerRecord("tool-results", results)
	providerTap(tap, 2,
		map[string]any{"type": "message_start", "message": map[string]any{"id": "native_done", "role": "assistant", "model": "claude-haiku", "usage": map[string]int{"input_tokens": 8}}},
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "tools-complete"}},
		map[string]any{"type": "content_block_stop", "index": 0},
		map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 2}},
		map[string]any{"type": "message_stop"})
	providerTapEnd(tap, 2)
	providerTranscript(dir, sid,
		map[string]any{"type": "assistant", "requestId": "request_done", "message": map[string]any{"id": "native_done", "model": "claude-haiku", "role": "assistant", "content": []any{map[string]string{"type": "text", "text": "tools-complete"}}}},
		map[string]any{"type": "system", "subtype": "turn_duration"})
	return true
}
