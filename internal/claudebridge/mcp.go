// Package claudebridge contains the stdio MCP bridge used only by an owned
// native Claude provider session. Adapted under MIT from yetone/magpie
// 1e174f8f9f613a920203a4658d259456fcab5d0d, internal/claudebridge/mcp.go.
package claudebridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

const maximum = 8 << 20

var callbackPath = regexp.MustCompile(`^/tool/[0-9a-f]{64}$`)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func privateBytes(path string, max int) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("MCP private input is unavailable")
	}
	for p := path; ; p = filepath.Dir(p) {
		i, err := os.Lstat(p)
		if err != nil || i.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("MCP private input is unavailable")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	i, err := os.Lstat(path)
	if err != nil || !i.Mode().IsRegular() || !privatefile.Check(path) {
		return nil, errors.New("MCP private input is unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("MCP private input is unavailable")
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(i, actual) {
		return nil, errors.New("MCP private input changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max+1)))
	if err != nil || len(b) > max {
		return nil, errors.New("MCP private input exceeds its limit")
	}
	return b, nil
}

// RunMCP takes exactly three private absolute filenames. Unlike the upstream
// generic bridge, it accepts no URL or environment fallback and no tool-list
// mutation. The sole HTTP target is the session's unguessable loopback callback.
func RunMCP(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 3 {
		return errors.New("MCP helper requires three explicit private files")
	}
	b, err := privateBytes(args[0], 512)
	if err != nil {
		return err
	}
	u, err := url.Parse(string(bytes.TrimSpace(b)))
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !callbackPath.MatchString(u.Path) {
		return errors.New("MCP callback is not an owned loopback endpoint")
	}
	data, err := privateBytes(args[1], 512<<10)
	if err != nil {
		return err
	}
	var tools []Tool
	if json.Unmarshal(data, &tools) != nil || len(tools) > 64 {
		return errors.New("MCP tool catalog is invalid")
	}
	names := map[string]bool{}
	for _, t := range tools {
		if t.Name == "" || names[t.Name] {
			return errors.New("MCP tool catalog is invalid")
		}
		names[t.Name] = true
	}
	if filepath.Dir(args[0]) != filepath.Dir(args[1]) || filepath.Dir(args[0]) != filepath.Dir(args[2]) || !privatefile.CheckDirectory(filepath.Dir(args[0])) {
		return errors.New("MCP files must belong to the same private session")
	}
	if _, err := os.Lstat(args[2]); !errors.Is(err, os.ErrNotExist) {
		return errors.New("MCP ready marker already exists")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxConnsPerHost = 64
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 130 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirect is forbidden") }}
	enc := json.NewEncoder(out)
	var output sync.Mutex
	var listed sync.Once
	var calls sync.WaitGroup
	respond := func(v any) { output.Lock(); defer output.Unlock(); _ = enc.Encode(v) }
	sem := make(chan struct{}, 64)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maximum)
	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.JSONRPC != "2.0" {
			return errors.New("MCP request is invalid")
		}
		if len(req.ID) == 0 {
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			return errors.New("MCP pending call limit reached")
		}
		calls.Add(1)
		go func(req request) {
			defer calls.Done()
			defer func() { <-sem }()
			var result any
			var failure string
			switch req.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "magpie", "version": "1"}}
			case "ping":
				result = map[string]any{}
			case "tools/list":
				result = map[string]any{"tools": tools}
			case "tools/call":
				var p struct {
					Name      string                     `json:"name"`
					Arguments json.RawMessage            `json:"arguments"`
					Meta      map[string]json.RawMessage `json:"_meta"`
				}
				if json.Unmarshal(req.Params, &p) != nil || !names[p.Name] {
					failure = "MCP tool is not registered"
					break
				}
				var id string
				if json.Unmarshal(p.Meta["claudecode/toolUseId"], &id) != nil || id == "" || len(id) > 256 {
					failure = "Claude tool identity is unavailable"
					break
				}
				var arguments map[string]json.RawMessage
				if json.Unmarshal(p.Arguments, &arguments) != nil || arguments == nil {
					failure = "MCP arguments are invalid"
					break
				}
				payload, _ := json.Marshal(map[string]any{"tool_call_id": id, "name": p.Name, "arguments": p.Arguments})
				r, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
				r.Header.Set("Content-Type", "application/json")
				response, err := client.Do(r)
				if err != nil {
					failure = "MCP session ended"
					break
				}
				body, readErr := io.ReadAll(io.LimitReader(response.Body, maximum+1))
				response.Body.Close()
				if readErr != nil || len(body) > maximum || response.StatusCode != 200 {
					failure = "MCP tool continuation was rejected"
					break
				}
				var answer struct {
					Content json.RawMessage `json:"content"`
					IsError bool            `json:"isError"`
				}
				if json.Unmarshal(body, &answer) != nil || len(answer.Content) == 0 {
					failure = "MCP tool result is invalid"
					break
				}
				result = map[string]any{"content": answer.Content, "isError": answer.IsError}
			default:
				failure = "MCP method is unsupported"
			}
			message := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			if failure != "" {
				message["error"] = map[string]any{"code": -32000, "message": failure}
			} else {
				message["result"] = result
			}
			respond(message)
			if req.Method == "tools/list" && failure == "" {
				listed.Do(func() {
					f, err := os.OpenFile(args[2], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if err == nil {
						_ = privatefile.Protect(args[2], 0600)
						_ = f.Sync()
						_ = f.Close()
					}
				})
			}
		}(req)
	}
	cancel()
	// Every HTTP callback is tied to ctx, so EOF cancels a parked tool wait.
	done := make(chan struct{})
	go func() { calls.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	return scanner.Err()
}
