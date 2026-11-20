package community

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"
)

// CodexHTTPBinding names gateway-owned objects, never upstream URLs or account
// tokens. The native plugin resolves provider/account/model relationships from
// the authenticated host's loaded configuration for each metadata request.
type CodexHTTPBinding struct {
	Provider string   `json:"provider"`
	Routes   []string `json:"routes"`
}
type CodexHTTPConfig struct {
	Enabled  bool                        `json:"enabled"`
	Loopback *CodexHTTPBinding           `json:"loopback,omitempty"`
	Callers  map[string]CodexHTTPBinding `json:"callers,omitempty"`
}

func codexName(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (c CodexHTTPConfig) validate() error {
	validateBinding := func(b CodexHTTPBinding) error {
		if !codexName(b.Provider, 128) || len(b.Routes) == 0 || len(b.Routes) > 4096 {
			return errors.New("Codex binding needs one local provider and 1..4096 route IDs")
		}
		seen := map[string]bool{}
		for _, route := range b.Routes {
			if !codexName(route, 512) || seen[route] {
				return errors.New("Codex binding contains an invalid or duplicate route ID")
			}
			seen[route] = true
		}
		return nil
	}
	if c.Enabled && c.Loopback == nil && len(c.Callers) == 0 {
		return errors.New("enabled Codex metadata needs an explicit loopback or caller-key binding")
	}
	if c.Loopback != nil {
		if err := validateBinding(*c.Loopback); err != nil {
			return err
		}
	}
	if len(c.Callers) > 1024 {
		return errors.New("Codex metadata allows at most 1024 caller-key bindings")
	}
	for id, binding := range c.Callers {
		if !codexName(id, 256) {
			return errors.New("invalid Codex caller-key ID")
		}
		if err := validateBinding(binding); err != nil {
			return err
		}
	}
	return nil
}

// CodexHTTP changes only an explicit local routing policy. It does not read
// credentials, start a host, refresh quota, or import an account.
func CodexHTTP(root, action, file string) (map[string]any, error) {
	if action == "show" {
		r, err := Read(root)
		if err != nil {
			return nil, err
		}
		config := r.CodexHTTP
		if config == nil {
			config = &CodexHTTPConfig{}
		}
		return map[string]any{"codex_http": config, "read_only": true}, nil
	}
	if action != "configure" && action != "disable" {
		return nil, errors.New("codex expects show, configure --file JSON, or disable")
	}
	var config CodexHTTPConfig
	if action == "configure" {
		if file == "" {
			return nil, errors.New("codex configure requires --file JSON")
		}
		data, err := readLimit(file, 4<<20)
		if err != nil {
			return nil, err
		}
		var shape map[string]json.RawMessage
		if json.Unmarshal(data, &shape) != nil || shape == nil || (string(bytes.TrimSpace(shape["enabled"])) != "true" && string(bytes.TrimSpace(shape["enabled"])) != "false") {
			return nil, errors.New("Codex binding must be an object with an explicit enabled boolean")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&config); err != nil {
			return nil, errors.New("invalid Codex metadata binding JSON")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, errors.New("Codex binding has trailing JSON")
		}
		if err = config.validate(); err != nil {
			return nil, err
		}
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
	if action == "disable" && r.CodexHTTP != nil {
		config = *r.CodexHTTP
		config.Enabled = false
	}
	r.CodexHTTP = &config
	if err = Save(root, r); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": true, "codex_http": config, "restart_required": true,
		"message": "Rebind the installed runtime with plugins runtime configure, then restart the gateway; no credentials were renewed",
	}, nil
}
