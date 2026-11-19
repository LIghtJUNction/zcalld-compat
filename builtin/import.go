// Package builtin migrates credentials without invoking a provider, a model, or
// a token endpoint. Source mappings are private and support an explicit rollback
// sync after both gateways have stopped using the sign-in.
package builtin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/community"
)

const maxFile = 8 << 20

type object = map[string]any
type Source struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	LoginID    string `json:"login_id,omitempty"`
	UserHash   string `json:"user_hash,omitempty"`
	Entry      string `json:"entry,omitempty"`
	SecretHash string `json:"secret_hash"`
}
type Account struct {
	Key          string   `json:"key"`
	Agent        string   `json:"agent"`
	Enabled      bool     `json:"enabled"`
	ImportedHash string   `json:"imported_hash"`
	Sources      []Source `json:"sources"`
}
type Mapping struct {
	Version  int       `json:"version"`
	Source   string    `json:"source"`
	Accounts []Account `json:"accounts"`
}
type activePaths struct{ CodexHome, ConfigHome, AppData string }

func str(v any) string     { s, _ := v.(string); return s }
func obj(v any) object     { o, _ := v.(map[string]any); return o }
func truth(v any) bool     { b, _ := v.(bool); return b }
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func digest(v any) string  { b, _ := json.Marshal(v); return hash(string(b)) }
func tokenHash(a object) string {
	if r := str(a["refresh"]); r != "" {
		return hash(r)
	}
	return hash(str(a["access"]))
}

func read(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return errors.New("credential source is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return err
	}
	if len(b) > maxFile {
		return errors.New("credential file exceeds 8 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(target) != nil {
		return errors.New("credential file contains invalid JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("credential file has trailing JSON")
	}
	return nil
}
func save(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxFile {
		return errors.New("credential output exceeds 8 MiB")
	}
	return community.Atomic(path, append(b, '\n'), 0600)
}
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
func number(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		x, _ := n.Int64()
		return x
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}
func claims(token string) object {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 32768 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var o object
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&o) != nil {
		return nil
	}
	return o
}
func validToken(s string) bool {
	return s != "" && len(s) <= 32768 && !strings.ContainsAny(s, "\r\n\x00\t ")
}
func decodeAuth(v any) object {
	if o := obj(v); o != nil {
		return o
	}
	if text := str(v); text != "" {
		var o object
		d := json.NewDecoder(strings.NewReader(text))
		d.UseNumber()
		if d.Decode(&o) == nil {
			return o
		}
	}
	return nil
}

func normalized(agent string, raw object) object {
	if raw == nil {
		return nil
	}
	var a object
	switch agent {
	case "codex":
		t := obj(raw["tokens"])
		access := str(t["access_token"])
		if !validToken(access) {
			return nil
		}
		id := str(t["account_id"])
		if id == "" {
			id = str(obj(claims(str(t["id_token"]))["https://api.openai.com/auth"])["chatgpt_account_id"])
		}
		a = object{"type": "oauth", "access": access, "refresh": str(t["refresh_token"]), "expires": number(claims(access)["exp"]) * 1000, "accountId": id, "idToken": str(t["id_token"])}
	case "copilot":
		access := str(raw["oauth_token"])
		if access == "" {
			access = str(raw["access_token"])
		}
		if !validToken(access) {
			return nil
		}
		kind := "editor"
		if truth(raw["cli"]) || str(raw["tokenKind"]) == "cli" {
			kind = "cli"
		}
		a = object{"type": "oauth", "access": access, "refresh": "", "expires": 0, "tokenKind": kind}
		if host := str(raw["host"]); host != "" {
			a["host"] = host
		}
	case "antigravity", "gemini":
		access := str(raw["access_token"])
		refresh := str(raw["refresh_token"])
		if !validToken(access) && !validToken(refresh) {
			return nil
		}
		a = object{"type": "oauth", "access": access, "refresh": refresh, "expires": number(raw["expiry_date"])}
		if p := str(raw["project"]); p != "" {
			a["project"] = p
		}
	default:
		return nil
	}
	if r := str(a["refresh"]); r != "" && !validToken(r) {
		return nil
	}
	return a
}

func keyFor(agent, id string, a object) string {
	if id == "" {
		id = str(a["accountId"])
		if id == "" {
			id = tokenHash(a)
		}
	}
	return "builtin:" + agent + "#" + hash(agent + ":" + id)[:20]
}
func addSource(account *Account, source Source) {
	for _, old := range account.Sources {
		if old.Path == source.Path && old.Kind == source.Kind && old.Entry == source.Entry && old.LoginID == source.LoginID {
			return
		}
	}
	account.Sources = append(account.Sources, source)
}

// Import refreshes a staged copy only while that copy still equals its imported
// generation. It refuses to overwrite credentials that the new host has renewed.
// The old gateway and its files are never modified by this function.
func Import(root, source string) (map[string]any, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := activePaths{CodexHome: os.Getenv("CODEX_HOME"), ConfigHome: os.Getenv("XDG_CONFIG_HOME"), AppData: os.Getenv("APPDATA")}
	if paths.CodexHome == "" {
		paths.CodexHome = filepath.Join(home, ".codex")
	}
	if paths.ConfigHome == "" {
		paths.ConfigHome = filepath.Join(home, ".config")
	}
	return importPaths(root, source, paths)
}

func importPaths(root, source string, paths activePaths) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) {
		return nil, errors.New("builtin import paths must be absolute")
	}
	source, err := canonical(source)
	if err != nil {
		return nil, err
	}
	if rel, e := filepath.Rel(source, root); e == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
		return nil, errors.New("builtin import destination must be outside Magpie")
	}
	unlock, err := community.Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	authPath := filepath.Join(root, "plugin-auth.json")
	authUnlock, err := claimAuth(authPath)
	if err != nil {
		return nil, err
	}
	defer authUnlock()
	auth := object{}
	if err = read(authPath, &auth); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("credential file must contain an object")
	}
	var previous Mapping
	mapPath := filepath.Join(root, "builtin-auth-map.json")
	if err = read(mapPath, &previous); err == nil {
		if previous.Version != 1 || previous.Source != source {
			return nil, errors.New("builtin import belongs to a different source")
		}
		for _, old := range previous.Accounts {
			if digest(auth[old.Key]) != old.ImportedHash {
				return nil, errors.New("staged builtin credentials changed; sync them before reimporting")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var logins []object
	loginsPath, err := canonical(filepath.Join(source, "logins.json"))
	if err != nil {
		return nil, err
	}
	if err = read(loginsPath, &logins); err != nil {
		return nil, err
	}
	// Explicitly supplied application identities stay in this private directory.
	// They are never compiled into adapters, printed, or taken from the network.
	clients := object{}
	clientPath := filepath.Join(root, "builtin-oauth-clients.json")
	if st, e := os.Stat(clientPath); e == nil {
		if runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
			return nil, errors.New("builtin OAuth client configuration must be private (0600)")
		}
		if e = read(clientPath, &clients); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	mapping := Mapping{Version: 1, Source: source, Accounts: []Account{}}
	accounts := map[string]*Account{}
	values := map[string]object{}
	loginUsers := map[string]map[string]string{}
	skipped := 0
	missingClients := 0
	for _, login := range logins {
		agent := str(login["agent"])
		if !strings.Contains("|codex|copilot|antigravity|gemini|", "|"+agent+"|") {
			if agent == "claude" {
				skipped++
			}
			continue
		}
		userHash := hash(str(login["user"]))
		if loginUsers[agent] == nil {
			loginUsers[agent] = map[string]string{}
		}
		loginUsers[agent][userHash] = str(login["id"])
		a := normalized(agent, decodeAuth(login["auth"]))
		if a == nil {
			continue
		}
		if agent == "gemini" || agent == "antigravity" {
			client := obj(clients[agent])
			raw := decodeAuth(login["auth"])
			for _, field := range []string{"clientId", "clientSecret"} {
				if value := str(raw[field]); value != "" {
					a[field] = value
				} else if value = str(client[field]); value != "" {
					a[field] = value
				}
			}
			if !validToken(str(a["clientId"])) || !validToken(str(a["clientSecret"])) {
				missingClients++
			}
		}
		if p := str(login["project"]); p != "" {
			a["project"] = p
		}
		key := keyFor(agent, str(login["id"]), a)
		src := Source{Path: loginsPath, Kind: "logins", LoginID: str(login["id"]), UserHash: userHash, SecretHash: tokenHash(a)}
		account := &Account{Key: key, Agent: agent, Enabled: truth(login["on"]) && !truth(login["paused"]) && str(login["lapsed"]) == "", Sources: []Source{src}}
		accounts[key], values[key] = account, a
	}
	// Agent-owned credentials are an active sign-in even when the corresponding
	// remembered Magpie login has on=false or auth=null.
	if p, e := canonical(filepath.Join(paths.CodexHome, "auth.json")); e == nil {
		var raw object
		if e = read(p, &raw); e != nil {
			return nil, e
		}
		if a := normalized("codex", raw); a != nil {
			var match *Account
			for key, item := range accounts {
				if item.Agent == "codex" && ((str(a["accountId"]) != "" && a["accountId"] == values[key]["accountId"]) || tokenHash(a) == tokenHash(values[key])) {
					match = item
					break
				}
			}
			if match == nil {
				key := keyFor("codex", "", a)
				match = &Account{Key: key, Agent: "codex"}
				accounts[key] = match
			}
			// The active file is authoritative; a stale saved copy is left mapped
			// to its own old fingerprint, so rollback can safely update both.
			match.Enabled = true
			values[match.Key] = a
			addSource(match, Source{Path: p, Kind: "codex", SecretHash: tokenHash(a)})
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	copilotCandidates := []string{filepath.Join(paths.ConfigHome, "github-copilot", "apps.json"), filepath.Join(paths.ConfigHome, "github-copilot", "hosts.json")}
	if paths.AppData != "" {
		copilotCandidates = append(copilotCandidates, filepath.Join(paths.AppData, "github-copilot", "apps.json"), filepath.Join(paths.AppData, "github-copilot", "hosts.json"))
	}
	for _, path := range copilotCandidates {
		p, e := canonical(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		var apps object
		if e = read(p, &apps); e != nil {
			return nil, e
		}
		appKeys := make([]string, 0, len(apps))
		for key := range apps {
			appKeys = append(appKeys, key)
		}
		sort.Strings(appKeys)
		for _, entry := range appKeys {
			raw := obj(apps[entry])
			a := normalized("copilot", raw)
			if a == nil {
				continue
			}
			host := strings.SplitN(entry, ":", 2)[0]
			if host != "github.com" {
				a["host"] = host
			}
			userHash := hash(str(raw["user"]))
			id := loginUsers["copilot"][userHash]
			if id == "" {
				id = host + ":" + str(raw["user"])
			}
			key := keyFor("copilot", id, a)
			account := accounts[key]
			if account == nil {
				account = &Account{Key: key, Agent: "copilot"}
				accounts[key] = account
				values[key] = a
			}
			// apps.json wins over hosts.json for a duplicate account, as Magpie
			// does; both original locations remain part of the private map.
			active := false
			for _, s := range account.Sources {
				if s.Kind == "copilot-app" {
					active = true
				}
			}
			if !active {
				values[key] = a
			}
			account.Enabled = true
			addSource(account, Source{Path: p, Kind: "copilot-app", Entry: entry, UserHash: userHash, SecretHash: tokenHash(a)})
		}
	}
	for key, account := range accounts {
		if _, exists := auth[key]; exists {
			known := false
			for _, old := range previous.Accounts {
				if old.Key == key {
					known = true
					break
				}
			}
			if !known {
				return nil, errors.New("builtin account key already belongs to another import")
			}
		}
		auth[key] = values[key]
		account.ImportedHash = digest(values[key])
		mapping.Accounts = append(mapping.Accounts, *account)
	}
	sort.Slice(mapping.Accounts, func(i, j int) bool { return mapping.Accounts[i].Key < mapping.Accounts[j].Key })
	resources, err := community.BuiltinScripts(root)
	if err != nil {
		return nil, err
	}
	registry, err := community.Read(root)
	if err != nil {
		return nil, err
	}
	var templates []object
	if err = read(resources["catalog.json"], &templates); err != nil {
		return nil, err
	}
	providerConfig := obj(registry.Config["provider"])
	if registry.Config == nil {
		registry.Config = object{}
	}
	if providerConfig == nil {
		providerConfig = object{}
		registry.Config["provider"] = providerConfig
	}
	registered := map[string]bool{}
	for _, entry := range registry.Plugins {
		registered[entry.Name] = true
	}
	var catalog []object
	enabled := 0
	for _, account := range mapping.Accounts {
		if !account.Enabled {
			continue
		}
		enabled++
		module := account.Agent + ".mjs"
		if account.Agent == "gemini" {
			module = "codeassist.mjs"
		}
		path := resources[module]
		if path == "" {
			return nil, fmt.Errorf("builtin adapter is unavailable for %s", account.Agent)
		}
		name := "builtin-" + account.Agent
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		entry := community.Entry{Name: name, Path: path, Kind: "community", Enabled: true, Source: name, SHA256: hash(string(data)), Version: "0.1.0"}
		if !registered[name] {
			registry.Plugins = append(registry.Plugins, entry)
			registered[name] = true
		} else {
			for i := range registry.Plugins {
				if registry.Plugins[i].Name == name {
					registry.Plugins[i] = entry
				}
			}
		}
		for _, template := range templates {
			if str(template["sourceId"]) != account.Agent {
				continue
			}
			copy := object{}
			for k, v := range template {
				copy[k] = v
			}
			copy["accountId"] = account.Key
			catalog = append(catalog, copy)
			models := object{}
			if list, ok := template["models"].([]any); ok {
				for _, v := range list {
					m := obj(v)
					models[str(m["apiId"])] = m
				}
			}
			providerConfig["builtin:"+account.Agent] = object{"name": account.Agent, "npm": template["npm"], "models": models}
		}
	}
	for _, old := range previous.Accounts {
		if _, ok := accounts[old.Key]; !ok {
			delete(auth, old.Key)
		}
	}
	if err = save(authPath, auth); err != nil {
		return nil, err
	}
	if err = save(mapPath, mapping); err != nil {
		return nil, err
	}
	catalogPath := filepath.Join(root, "builtin-providers.json")
	if err = save(catalogPath, catalog); err != nil {
		return nil, err
	}
	if err = community.Save(root, registry); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "builtin_accounts": len(mapping.Accounts), "enabled_accounts": enabled, "unsupported_accounts": skipped, "google_client_setup_required": missingClients, "catalog_file": catalogPath, "network_requests": 0}, nil
}

func claimAuth(path string) (func(), error) {
	locks, err := filepath.Glob(path + ".renew-*.lock")
	if err != nil {
		return nil, err
	}
	if len(locks) > 0 {
		return nil, errors.New("an account refresh is in progress; finish the credential handoff first")
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("account file is busy; finish the credential handoff first")
	}
	f.Close()
	return func() { _ = os.Remove(path + ".lock") }, nil
}

func findLogin(logins []object, s Source, agent string) (object, error) {
	var match object
	for _, login := range logins {
		if str(login["agent"]) != agent {
			continue
		}
		if (s.LoginID != "" && str(login["id"]) == s.LoginID) || (s.LoginID == "" && hash(str(login["user"])) == s.UserHash) {
			if match != nil {
				return nil, errors.New("credential source account is ambiguous")
			}
			match = login
		}
	}
	if match == nil {
		return nil, errors.New("credential source account was removed")
	}
	return match, nil
}
func patch(agent string, raw, a object) error {
	if !validToken(str(a["access"])) {
		return errors.New("renewed account has no valid access token")
	}
	switch agent {
	case "codex":
		t := obj(raw["tokens"])
		if t == nil {
			return errors.New("Codex credential structure changed")
		}
		if id := str(t["account_id"]); id != "" && str(a["accountId"]) != "" && id != str(a["accountId"]) {
			return errors.New("Codex account identity changed")
		}
		t["access_token"] = a["access"]
		if str(a["refresh"]) != "" {
			t["refresh_token"] = a["refresh"]
		}
		if str(a["idToken"]) != "" {
			t["id_token"] = a["idToken"]
		}
		if str(a["accountId"]) != "" {
			t["account_id"] = a["accountId"]
		}
		raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	case "copilot":
		if _, ok := raw["oauth_token"]; ok {
			raw["oauth_token"] = a["access"]
		} else {
			raw["access_token"] = a["access"]
		}
	case "antigravity", "gemini":
		raw["access_token"] = a["access"]
		if str(a["refresh"]) != "" {
			raw["refresh_token"] = a["refresh"]
		}
		raw["expiry_date"] = a["expires"]
		if str(a["project"]) != "" {
			raw["project"] = a["project"]
		}
	default:
		return errors.New("unsupported credential sync provider")
	}
	return nil
}

// Sync writes only the mapped token fields back for an explicit rollback. It
// prevalidates every source against the original or already-synced secret hash.
// A new login/refresh made by another owner is never overwritten. Stop both
// gateways and other owners first: these are different files, not one lock domain.
func Sync(root, source string) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(source) {
		return nil, errors.New("auth sync paths must be absolute")
	}
	source, err := canonical(source)
	if err != nil {
		return nil, err
	}
	unlock, err := community.Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var mapping Mapping
	if err = read(filepath.Join(root, "builtin-auth-map.json"), &mapping); err != nil {
		return nil, err
	}
	if mapping.Version != 1 || mapping.Source != source {
		return nil, errors.New("auth sync source does not match the private migration map")
	}
	authPath := filepath.Join(root, "plugin-auth.json")
	release, err := claimAuth(authPath)
	if err != nil {
		return nil, err
	}
	defer release()
	var auth object
	if err = read(authPath, &auth); err != nil {
		return nil, err
	}
	documents := map[string]any{}
	before := map[string]string{}
	changed := 0
	for _, account := range mapping.Accounts {
		a := obj(auth[account.Key])
		if a == nil {
			return nil, errors.New("migrated builtin account was removed")
		}
		for _, s := range account.Sources {
			if !filepath.IsAbs(s.Path) {
				return nil, errors.New("invalid credential source mapping")
			}
			if _, ok := documents[s.Path]; !ok {
				var doc any
				if err = read(s.Path, &doc); err != nil {
					return nil, err
				}
				documents[s.Path] = doc
				before[s.Path] = digest(doc)
			}
			var raw object
			var login object
			wasString := false
			switch s.Kind {
			case "logins":
				items, ok := documents[s.Path].([]any)
				if !ok {
					return nil, errors.New("Magpie login format changed")
				}
				ls := make([]object, 0, len(items))
				for _, item := range items {
					if o := obj(item); o != nil {
						ls = append(ls, o)
					}
				}
				login, err = findLogin(ls, s, account.Agent)
				if err != nil {
					return nil, err
				}
				_, wasString = login["auth"].(string)
				raw = decodeAuth(login["auth"])
			case "codex":
				raw = obj(documents[s.Path])
			case "copilot-app":
				raw = obj(obj(documents[s.Path])[s.Entry])
				if raw != nil && hash(str(raw["user"])) != s.UserHash {
					return nil, errors.New("Copilot source account identity changed")
				}
			default:
				return nil, errors.New("invalid credential source kind")
			}
			old := normalized(account.Agent, raw)
			if old == nil {
				return nil, errors.New("credential source is no longer a compatible sign-in")
			}
			if tokenHash(old) != s.SecretHash && tokenHash(old) != tokenHash(a) {
				return nil, errors.New("credential source was renewed or replaced by another owner; refusing rollback overwrite")
			}
			if err = patch(account.Agent, raw, a); err != nil {
				return nil, err
			}
			if login != nil {
				if wasString {
					b, _ := json.Marshal(raw)
					login["auth"] = string(b)
				} else {
					login["auth"] = raw
				}
			}
		}
	}
	paths := make([]string, 0, len(documents))
	for path := range documents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// Re-read every source before writing any, so a concurrent owner observed
	// during preparation causes a clean refusal. Callers still stop token owners.
	for _, path := range paths {
		var current any
		if err = read(path, &current); err != nil {
			return nil, err
		}
		if digest(current) != before[path] {
			return nil, errors.New("credential source changed during rollback preparation")
		}
	}
	for _, path := range paths {
		if err = save(path, documents[path]); err != nil {
			return nil, err
		}
		changed++
	}
	return map[string]any{"ok": true, "synced_accounts": len(mapping.Accounts), "updated_files": changed, "network_requests": 0}, nil
}
