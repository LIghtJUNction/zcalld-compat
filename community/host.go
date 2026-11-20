package community

import (
	"bufio"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

//go:embed vendor/host.js vendor/pi.js vendor/mcp-tools.mjs
var scripts embed.FS

type Host struct {
	cmd           *exec.Cmd
	cancel        context.CancelFunc
	input         io.WriteCloser
	scan          *bufio.Scanner
	output        io.Reader
	closed        sync.Once
	next          uint64
	drainDeadline time.Time
	drainGrace    time.Duration
}

func Scripts(root string) (host, pi string, err error) {
	names := []string{"host.js", "pi.js", "mcp-tools.mjs"}
	contents := make(map[string][]byte, len(names))
	sum := sha256.New()
	for _, name := range names {
		b, e := scripts.ReadFile("vendor/" + name)
		if e != nil {
			err = e
			return
		}
		contents[name] = b
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write(b)
	}
	directory := filepath.Join(root, "runtime", hex.EncodeToString(sum.Sum(nil)[:16]))
	for _, name := range names {
		b := contents[name]
		p := filepath.Join(directory, name)
		if old, e := os.ReadFile(p); e != nil || string(old) != string(b) {
			if e = Atomic(p, b, 0600); e != nil {
				err = e
				return
			}
		}
	}
	return filepath.Join(directory, "host.js"), filepath.Join(directory, "pi.js"), nil
}
func Start(root string) (*Host, error) {
	return startHost(root, "", time.Time{})
}

func startHost(root, source string, deadline time.Time) (*Host, error) {
	return startHostBudget(root, source, deadline, time.Time{})
}

func startHostBudget(root, source string, deadline, drainDeadline time.Time) (*Host, error) {
	r, err := Read(root)
	if err != nil {
		return nil, err
	}
	host, pi, err := Scripts(root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bun", host)
	cmd.Dir = root
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("Bun is required for this community plugin operation")
	}
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 65536), 16<<20)
	h := &Host{cmd: cmd, cancel: cancel, input: in, scan: scan, output: out, drainDeadline: drainDeadline}
	items := []any{}
	for _, p := range r.Plugins {
		if p.Enabled && p.Kind == "community" && (source == "" || p.Source == source) {
			items = append(items, map[string]any{"spec": p.Source, "target": p.Path, "options": p.Options, "gateway": p.Gateway})
		}
	}
	params := map[string]any{"authPath": filepath.Join(root, "plugin-auth.json"), "registryPath": filepath.Join(root, "registry.json"), "piPath": pi, "directory": root, "plugins": items, "config": r.Config, "prefer": r.Prefer, "options": r.ProviderOptions, "modelsDevPath": r.ModelsDevPath}
	var loaded struct {
		Plugins []struct {
			Error string `json:"error"`
		} `json:"plugins"`
	}
	if err = h.callUntil("init", params, &loaded, deadline); err != nil {
		h.Close()
		return nil, err
	}
	for _, p := range loaded.Plugins {
		if p.Error != "" {
			h.Close()
			return nil, errors.New("an enabled community plugin could not load")
		}
	}
	return h, nil
}
func (h *Host) Close() {
	h.closed.Do(func() {
		h.input.Close()
		go func() { _, _ = io.Copy(io.Discard, h.output) }()
		done := make(chan struct{})
		go func() { _ = h.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(h.drainRemaining()):
			h.cancel()
			<-done
		}
		h.cancel()
	})
}
func (h *Host) Call(method string, params, out any) error {
	h.next++
	id := h.next
	if err := json.NewEncoder(h.input).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return errors.New("community host ended")
	}
	for h.scan.Scan() {
		var answer struct {
			ID     uint64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(h.scan.Bytes(), &answer) != nil {
			return errors.New("community host protocol failure")
		}
		if answer.ID != id {
			continue
		}
		if len(answer.Error) > 0 && string(answer.Error) != "null" {
			return errors.New("community plugin operation failed")
		}
		if out == nil {
			return nil
		}
		if json.Unmarshal(answer.Result, out) != nil {
			return errors.New("invalid community result")
		}
		return nil
	}
	return errors.New("community host ended or exceeded its response bound")
}
func Providers(root string) (map[string]any, error) {
	h, e := Start(root)
	if e != nil {
		return nil, e
	}
	defer h.Close()
	var raw []map[string]any
	if e = h.Call("providers", map[string]any{}, &raw); e != nil {
		return nil, e
	}
	if e = saveQuotaCapabilities(root, raw); e != nil {
		return nil, errors.New("provider discovery completed but capability cache could not be saved")
	}
	out := []any{}
	for _, p := range raw {
		row := map[string]any{}
		for _, k := range []string{"id", "name", "spec", "npm", "baseURL", "api", "models", "methods", "usage"} {
			if v, ok := p[k]; ok {
				row[k] = v
			}
		}
		// Provider/model metadata contains no auth state. Strip any unexpected nested credentials.
		strip(row)
		out = append(out, row)
	}
	return map[string]any{"providers": out}, nil
}
func strip(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			switch k {
			case "apiKey", "key", "token", "access", "refresh", "cookie", "headers", "auth", "options":
				delete(v, k)
			default:
				strip(x)
			}
		}
	case []any:
		for _, x := range v {
			strip(x)
		}
	}
}
func decodeJSON(b []byte, v any) error { return json.Unmarshal(b, v) }
