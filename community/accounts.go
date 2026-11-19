// Account controls use the same private auth file as the live community host.
package community

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AccountMeta struct {
	Label   string `json:"label,omitempty"`
	Enabled bool   `json:"enabled"`
}
type AccountOptions struct {
	Provider string
	Label    string
	File     string
	ID       string
}

var providerPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}$")

func accountProvider(id string) string { return strings.SplitN(id, "#", 2)[0] }
func validAccount(id, provider string) bool {
	return len(id) <= 256 && (id == provider || strings.HasPrefix(id, provider+"#")) &&
		!strings.ContainsAny(id, "\r\n\t\x00 ") && providerPattern.MatchString(provider)
}
func readAuthFile(root string) (map[string]any, []byte, error) {
	b, err := readLimit(filepath.Join(root, "plugin-auth.json"), 8<<20)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, []byte("{}\n"), nil
	}
	if err != nil {
		return nil, nil, err
	}
	var all map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&all) != nil || all == nil {
		return nil, nil, errors.New("private account state is invalid")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, nil, errors.New("private account state has trailing JSON")
	}
	return all, b, nil
}
func seedAccounts(root string, r *Registry, all map[string]any) {
	if r.Accounts == nil {
		r.Accounts = map[string]AccountMeta{}
	}
	if r.AccountDefaults == nil {
		r.AccountDefaults = map[string]string{}
	}
	// An imported inactive builtin account remains inactive until explicitly enabled.
	flags := map[string]bool{}
	if b, e := readLimit(filepath.Join(root, "builtin-auth-map.json"), 8<<20); e == nil {
		var value struct {
			Accounts []struct {
				Key     string `json:"key"`
				Enabled bool   `json:"enabled"`
			} `json:"accounts"`
		}
		if json.Unmarshal(b, &value) == nil {
			for _, a := range value.Accounts {
				flags[a.Key] = a.Enabled
			}
		}
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := r.Accounts[id]; !ok {
			enabled := true
			if value, found := flags[id]; found {
				enabled = value
			}
			r.Accounts[id] = AccountMeta{Enabled: enabled}
		}
		provider := accountProvider(id)
		if r.AccountDefaults[provider] == "" && r.Accounts[id].Enabled {
			r.AccountDefaults[provider] = id
		}
	}
}

// ReconcileAccounts records existing account selection without invoking auth code.
func ReconcileAccounts(root string) error {
	unlock, e := Lock(root)
	if e != nil {
		return e
	}
	defer unlock()
	r, e := Read(root)
	if e != nil {
		return e
	}
	all, _, e := readAuthFile(root)
	if e != nil {
		return e
	}
	seedAccounts(root, &r, all)
	return Save(root, r)
}
func accountRows(r Registry, all map[string]any) []any {
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]any, 0, len(ids))
	for i, id := range ids {
		meta := r.Accounts[id]
		label := meta.Label
		if label == "" {
			label = id
		}
		auth, _ := all[id].(map[string]any)
		kind, _ := auth["type"].(string)
		out = append(out, map[string]any{"id": id, "index": i, "provider": accountProvider(id), "label": label,
			"enabled": meta.Enabled, "selected": r.AccountDefaults[accountProvider(id)] == id, "has_credentials": len(auth) > 0, "type": kind, "refresh_owner": func() string {
				if auth["refreshOwner"] == "external" {
					return "external"
				}
				return "plugin"
			}()})
	}
	return out
}

// authLock uses the upstream host's exclusive lock file, so refresh and edits cannot clobber each other.
func authLock(root string) (func(), error) {
	path := filepath.Join(root, "plugin-auth.json.lock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		if time.Now().After(deadline) {
			return nil, errors.New("account state is busy with another auth update")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func importAccountAuth(file string) (map[string]any, error) {
	if !filepath.IsAbs(file) {
		return nil, errors.New("--file must be an absolute private credential file")
	}
	b, e := readLimit(file, 8<<20)
	if e != nil {
		return nil, e
	}
	var auth map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&auth) != nil || auth == nil {
		return nil, errors.New("credential import needs one JSON object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("credential import has trailing JSON")
	}
	text := func(key string) string { v, _ := auth[key].(string); return v }
	kind := text("type")
	if kind == "" {
		if text("access") != "" || text("refresh") != "" {
			kind = "oauth"
		} else if text("key") != "" {
			kind = "api"
		}
		auth["type"] = kind
	}
	if (kind == "api" && text("key") == "") || (kind == "oauth" && text("access") == "" && text("refresh") == "") || (kind != "api" && kind != "oauth") {
		return nil, errors.New("credentials require type api with key, or oauth with access/refresh")
	}
	return auth, nil
}
func Accounts(root string, args []string, o AccountOptions) (map[string]any, error) {
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	if action != "list" && action != "import" && action != "use" && action != "enable" && action != "disable" && action != "remove" {
		return nil, errors.New("accounts expects list, import, use, enable, disable or remove")
	}
	unlock, e := Lock(root)
	if e != nil {
		return nil, e
	}
	defer unlock()
	var authUnlock func()
	if action == "import" || action == "remove" {
		authUnlock, e = authLock(root)
		if e != nil {
			return nil, e
		}
		defer authUnlock()
	}
	r, e := Read(root)
	if e != nil {
		return nil, e
	}
	all, oldAuth, e := readAuthFile(root)
	if e != nil {
		return nil, e
	}
	seedAccounts(root, &r, all)
	if action == "list" {
		if len(args) > 1 {
			return nil, errors.New("accounts list takes no account")
		}
		return map[string]any{"accounts": accountRows(r, all)}, nil
	}
	id := o.ID
	if action == "import" {
		if len(args) > 1 {
			return nil, errors.New("accounts import takes --provider and --file")
		}
		if !providerPattern.MatchString(o.Provider) {
			return nil, errors.New("valid --provider is required")
		}
		if id == "" {
			b := make([]byte, 10)
			if _, e = rand.Read(b); e != nil {
				return nil, e
			}
			id = o.Provider + "#" + hex.EncodeToString(b)
		}
		if !validAccount(id, o.Provider) {
			return nil, errors.New("account ID must belong to the selected provider")
		}
		if len(o.Label) > 256 || strings.ContainsAny(o.Label, "\r\n\x00") {
			return nil, errors.New("invalid account label")
		}
		auth, e := importAccountAuth(o.File)
		if e != nil {
			return nil, e
		}
		if _, exists := all[id]; exists {
			return nil, errors.New("account ID already exists; import a new account before switching")
		}
		all[id] = auth
		r.Accounts[id] = AccountMeta{Label: o.Label, Enabled: true}
		if r.AccountDefaults[o.Provider] == "" {
			r.AccountDefaults[o.Provider] = id
		}
	} else {
		if len(args) != 2 {
			return nil, errors.New("account ID or index required")
		}
		id = args[1]
		if index, e := strconv.Atoi(id); e == nil {
			ids := make([]string, 0, len(all))
			for k := range all {
				ids = append(ids, k)
			}
			sort.Strings(ids)
			if index < 0 || index >= len(ids) {
				return nil, errors.New("account index out of range")
			}
			id = ids[index]
		}
		if _, ok := all[id]; !ok {
			return nil, errors.New("account not found")
		}
		meta := r.Accounts[id]
		switch action {
		case "use":
			if !meta.Enabled {
				return nil, errors.New("enable the account before selecting it")
			}
			r.AccountDefaults[accountProvider(id)] = id
		case "enable":
			meta.Enabled = true
			r.Accounts[id] = meta
		case "disable":
			meta.Enabled = false
			r.Accounts[id] = meta
		case "remove":
			delete(all, id)
			delete(r.Accounts, id)
			if r.AccountDefaults[accountProvider(id)] == id {
				delete(r.AccountDefaults, accountProvider(id))
			}
		}
	}
	changedAuth := action == "import" || action == "remove"
	if changedAuth {
		b, _ := json.MarshalIndent(all, "", "  ")
		if e = Atomic(filepath.Join(root, "plugin-auth.json"), append(b, '\n'), 0600); e != nil {
			return nil, e
		}
	}
	if e = Save(root, r); e != nil {
		if changedAuth {
			if restore := Atomic(filepath.Join(root, "plugin-auth.json"), oldAuth, 0600); restore != nil {
				return nil, errors.New("account metadata failed and credential rollback failed")
			}
		}
		return nil, e
	}
	result := map[string]any{"ok": true, "id": id, "provider": accountProvider(id), "action": action}
	if action == "use" {
		result["selection"] = map[string]any{"provider": accountProvider(id), "account": id}
	}
	if action != "remove" {
		result["enabled"] = r.Accounts[id].Enabled
	}
	return result, nil
}
