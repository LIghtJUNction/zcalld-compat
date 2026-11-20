package community

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MCPPermission contains authorization metadata only. Gateway credentials never
// enter the community host's request envelope or this derived permission map.
type MCPPermission struct {
	MCP            []string        `json:"mcp,omitempty"`
	Off            bool            `json:"off,omitempty"`
	ReadOnly       bool            `json:"read_only,omitempty"`
	Browser        bool            `json:"browser,omitempty"`
	Issuer         string          `json:"issuer,omitempty"`
	ExpiresUnixMS  int64           `json:"expires_unix_ms,omitempty"`
	MaxConcurrency int             `json:"max_concurrency,omitempty"`
	Limit          json.RawMessage `json:"limit,omitempty"`
	MoreLimits     json.RawMessage `json:"more_limits,omitempty"`
}

// toolsEnabled composes the original built-in switch without loading plugins.
// Its source is plugins.yml; served.json is provider serving state and unrelated.
func toolsEnabled(source string) (bool, error) {
	enabled, gateway := true, true
	b, err := readLimit(filepath.Join(source, "plugins.yml"), 1<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil {
		var node yaml.Node
		if yaml.Unmarshal(b, &node) != nil {
			return false, errors.New("Magpie built-in plugin configuration is invalid")
		}
		count, aliases := 0, 0
		var visit func(*yaml.Node, int, map[*yaml.Node]bool) error
		visit = func(n *yaml.Node, depth int, active map[*yaml.Node]bool) error {
			count++
			if depth > 32 || count > 4096 || active[n] {
				return errors.New("Magpie built-in plugin YAML exceeds structural limits")
			}
			active[n] = true
			defer delete(active, n)
			if n.Kind == yaml.AliasNode {
				aliases++
				if aliases > 64 || n.Alias == nil {
					return errors.New("Magpie built-in plugin YAML exceeds alias limits")
				}
				return visit(n.Alias, depth+1, active)
			}
			for _, c := range n.Content {
				if err := visit(c, depth+1, active); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(&node, 0, map[*yaml.Node]bool{}); err != nil {
			return false, err
		}
		var file struct {
			Preset  string `yaml:"preset"`
			Plugins []struct {
				ID      string `yaml:"id"`
				Enabled *bool  `yaml:"enabled"`
			} `yaml:"plugins"`
		}
		if node.Decode(&file) != nil {
			return false, errors.New("Magpie built-in plugin configuration cannot be decoded")
		}
		if file.Preset != "" && file.Preset != "all" && file.Preset != "essentials" {
			return false, errors.New("unknown Magpie built-in plugin preset")
		}
		// Both gateway and tools are Essential; neither preset turns them off.
		for _, row := range file.Plugins {
			if row.Enabled == nil {
				continue
			}
			if row.ID == "tools" {
				enabled = *row.Enabled
			}
			if row.ID == "gateway" {
				gateway = *row.Enabled
			}
		}
	}
	if !gateway {
		return false, nil
	}
	if enabled {
		return true, nil
	}
	// The original InUse rule keeps tools on when an enabled plugin last gave
	// tools. Read its persisted note, never initialize an installed plugin here.
	b, err = readLimit(filepath.Join(source, "plugin-tools.json"), 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var noted map[string][]string
	if json.Unmarshal(b, &noted) != nil {
		return false, errors.New("Magpie plugin tool note is invalid; effective switch is unknown")
	}
	b, err = readLimit(filepath.Join(source, "plugins.json"), 8<<20)
	if err != nil {
		return false, err
	}
	var list struct {
		Plugins []struct {
			Spec string `json:"spec"`
			Off  bool   `json:"off"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &list) != nil {
		return false, errors.New("Magpie plugin list is invalid")
	}
	for _, p := range list.Plugins {
		if !p.Off && len(noted[p.Spec]) > 0 {
			return true, nil
		}
	}
	return false, nil
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func cleanMCP(in []string) ([]string, error) {
	out, seen, none := []string{}, map[string]bool{}, false
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if v == "-" {
			none = true
			continue
		}
		server, tool, hasTool := strings.Cut(v, "/")
		if !mcpServerName.MatchString(server) || hasTool && (tool == "" || len(tool) > 128 || strings.TrimSpace(tool) != tool) {
			return nil, errors.New("invalid MCP permission entry")
		}
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	if len(out) > 500 {
		return nil, errors.New("too many MCP permission entries")
	}
	kept := out[:0]
	for _, v := range out {
		server, _, hasTool := strings.Cut(v, "/")
		if !hasTool || !seen[server] {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 && none {
		return []string{"-"}, nil
	}
	return kept, nil
}

func importMCPState(source string, r *Registry) error {
	enabled, err := toolsEnabled(source)
	if err != nil {
		return err
	}
	b, err := readLimit(filepath.Join(source, "caller-keys.json"), 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte("[]")
	} else if err != nil {
		return err
	}
	var keys []struct {
		ID             string          `json:"id"`
		MCP            []string        `json:"mcp"`
		Off            bool            `json:"off"`
		ReadOnly       bool            `json:"readOnly"`
		Browser        bool            `json:"browser"`
		Issuer         string          `json:"issuer"`
		Expires        string          `json:"expires"`
		MaxConcurrency int             `json:"maxConcurrency"`
		Limit          json.RawMessage `json:"limit"`
		More           json.RawMessage `json:"moreLimits"`
	}
	if json.Unmarshal(b, &keys) != nil || len(keys) > 1024 {
		return errors.New("invalid Magpie caller-key permission metadata")
	}
	rules := map[string]MCPPermission{}
	for _, key := range keys {
		if key.ID == "" || len(key.ID) > 128 {
			return errors.New("invalid caller-key identifier")
		}
		if _, ok := rules[key.ID]; ok {
			return errors.New("duplicate caller-key identifier")
		}
		limit := strings.TrimSpace(string(key.Limit))
		more := strings.TrimSpace(string(key.More))
		if limit != "" && limit != "null" || more != "" && more != "null" && more != "[]" {
			return errors.New("MCP caller-key token or currency budgets are not supported; import was refused without relaxing the restriction")
		}
		if key.MaxConcurrency < 0 || key.MaxConcurrency > 4096 {
			return errors.New("unsupported MCP concurrency limit")
		}
		grants, err := cleanMCP(key.MCP)
		if err != nil {
			return err
		}
		expires := int64(0)
		if key.Expires != "" {
			at, e := time.Parse(time.RFC3339Nano, key.Expires)
			if e != nil {
				return errors.New("invalid caller-key expiry")
			}
			if !at.IsZero() {
				expires = max(int64(1), at.UnixMilli())
			}
		}
		rules[key.ID] = MCPPermission{MCP: grants, Off: key.Off, ReadOnly: key.ReadOnly, Browser: key.Browser, Issuer: key.Issuer, ExpiresUnixMS: expires, MaxConcurrency: key.MaxConcurrency}
	}
	for id := range rules {
		seen := map[string]bool{}
		for key := id; key != ""; {
			if seen[key] || len(seen) >= 32 {
				return errors.New("caller-key issuer cycle or depth exceeds MCP limit")
			}
			seen[key] = true
			v, ok := rules[key]
			if !ok {
				return errors.New("caller-key issuer is unavailable")
			}
			key = v.Issuer
		}
	}
	r.MCPToolsEnabled = &enabled
	r.MCPPermissions = rules
	return nil
}

// ImportMCP updates only the derived switch and permission metadata in an
// already migrated private registry. It never reads or executes plugin code.
func ImportMCP(root, source string) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) || filepath.Clean(root) == filepath.Clean(source) {
		return nil, errors.New("MCP import requires separate absolute source and destination directories")
	}
	unlock, err := Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Read(root)
	if err != nil {
		return nil, err
	}
	if err = importMCPState(source, &r); err != nil {
		return nil, err
	}
	if err = Save(root, r); err != nil {
		return nil, err
	}
	return mcpSummary(r), nil
}

func mcpSummary(r Registry) map[string]any {
	enabled := r.MCPToolsEnabled == nil || *r.MCPToolsEnabled
	scoped := 0
	for _, v := range r.MCPPermissions {
		if len(v.MCP) > 0 {
			scoped++
		}
	}
	return map[string]any{"ok": true, "enabled": enabled, "caller_keys": len(r.MCPPermissions), "scoped_caller_keys": scoped, "path": "/mcp/zcalld/tools", "legacy_path": "/mcp/magpie/tools", "restart_required": false}
}

func MCP(root, action string) (map[string]any, error) {
	unlock, err := Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Read(root)
	if err != nil {
		return nil, err
	}
	switch action {
	case "show", "":
		return mcpSummary(r), nil
	case "enable", "disable":
		on := action == "enable"
		r.MCPToolsEnabled = &on
		if err = Save(root, r); err != nil {
			return nil, err
		}
		return mcpSummary(r), nil
	default:
		return nil, errors.New("mcp expects show, enable, or disable")
	}
}
