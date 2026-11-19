package community

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
)

var publicMethods = map[string]bool{
	"providers": true, "prompt": true, "validate": true, "authorize": true, "callback": true,
	"apiKey": true, "importFiles": true, "usage": true, "checkin": true, "check": true, "keepSignedIn": true,
	"tool": true, "search": true, "calls": true, "gatewayState": true, "setProviderOptions": true,
}

func Invoke(root, provider, event, file string) (map[string]any, error) {
	if !publicMethods[event] {
		return nil, errors.New("unsupported public community event")
	}
	params := map[string]any{}
	if file != "" {
		b, e := readLimit(file, 8<<20)
		if e != nil {
			return nil, e
		}
		if json.Unmarshal(b, &params) != nil {
			return nil, errors.New("event input must be one JSON object")
		}
	}
	if provider != "" {
		params["provider"] = provider
	}
	h, e := Start(root)
	if e != nil {
		return nil, e
	}
	defer h.Close()
	var result any
	if e = h.Call(event, params, &result); e != nil {
		return nil, e
	}
	strip(result)
	return map[string]any{"ok": true, "result": result}, nil
}

// Session keeps OAuth authorization callbacks in the same host that began them.
// Only explicit public methods are accepted; auth storage/raw loaders are private.
func Session(root string, in io.Reader, out io.Writer) error {
	h, e := Start(root)
	if e != nil {
		return e
	}
	defer h.Close()
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 65536), 8<<20)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		var r struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		answer := map[string]any{}
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			answer["error"] = "invalid session request"
		} else {
			answer["id"] = r.ID
			if !publicMethods[r.Method] {
				answer["error"] = "unsupported public community event"
			} else {
				var result any
				if e = h.Call(r.Method, r.Params, &result); e != nil {
					answer["error"] = "community operation failed"
				} else {
					strip(result)
					answer["result"] = result
				}
			}
		}
		if e = enc.Encode(answer); e != nil {
			return e
		}
	}
	return scan.Err()
}
func Verify(root, name string) (map[string]any, error) {
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	i := index(r, name)
	if i < 0 {
		return nil, errors.New("extension not found")
	}
	p := r.Plugins[i]
	if p.Kind != "native" {
		return nil, errors.New("verify currently requires a native package")
	}
	sum, e := fileHash(p.Path)
	if e != nil {
		return nil, e
	}
	if sum != p.SHA256 {
		return nil, errors.New("native extension checksum mismatch")
	}
	return map[string]any{"ok": true, "name": name, "sha256": sum}, nil
}
func ConfigureRuntime(root string) error {
	host, pi, e := Scripts(root)
	if e != nil {
		return e
	}
	bun, e := findBun()
	if e != nil {
		return e
	}
	config := map[string]any{"bun": bun, "host": host, "pi": pi, "registry": root + "/registry.json", "auth_path": root + "/plugin-auth.json"}
	b, _ := json.MarshalIndent(config, "", "  ")
	return Atomic(root+"/host.json", b, 0600)
}
func findBun() (string, error) {
	p, e := exec.LookPath("bun")
	if e != nil {
		return "", errors.New("Bun is required for community plugins")
	}
	return p, nil
}
