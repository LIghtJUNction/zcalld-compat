// Package claudemaintenance adapts explicitly bound Claude Code accounts to the
// existing optional maintenance scheduler. It never imports or refreshes tokens.
package claudemaintenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/maintenance"
)

var ErrConfiguration = errors.New("Claude maintenance binding is unavailable or changed")

const maxInput = 2 << 20

// Bindings is an explicit private input, not an account discovery operation.
// Profile is the native .claude.json, which may be outside ConfigDir for the
// default current account. Saved directories must already exist and be private.
type Bindings struct {
	Version        int       `json:"version"`
	Binary         string    `json:"claude_binary"`
	Helper         string    `json:"interactive_helper,omitempty"`
	TrustWorkspace bool      `json:"trust_workspace,omitempty"`
	Accounts       []Binding `json:"accounts"`
}
type Binding struct {
	LoginID   string `json:"login_id"`
	UserHash  string `json:"user_hash,omitempty"`
	ConfigDir string `json:"config_dir"`
	Profile   string `json:"profile"`
	Ownership string `json:"ownership"`
	Enabled   bool   `json:"enabled"`
}

type identity struct {
	Email        string `json:"emailAddress"`
	Organization string `json:"organizationUuid"`
	Account      string `json:"accountUuid"`
}

func (i identity) valid() bool {
	return strings.TrimSpace(i.Email) != "" && strings.TrimSpace(i.Organization) != "" && strings.TrimSpace(i.Account) != ""
}
func (i identity) digest() string {
	b, _ := json.Marshal([]string{strings.ToLower(strings.TrimSpace(i.Email)), i.Organization, i.Account})
	return digest(b)
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type account struct {
	ConfigDir   string  `json:"config_dir"`
	Profile     string  `json:"profile"`
	Ownership   string  `json:"ownership"`
	Identity    string  `json:"identity"`
	Automatic   bool    `json:"automatic"`
	Enabled     bool    `json:"enabled"`
	NoExtra     bool    `json:"no_extra"`
	ExtraShare  int     `json:"extra_share,omitempty"`
	ExtraAmount float64 `json:"extra_amount,omitempty"`
	Proxy       string  `json:"proxy,omitempty"`
}
type config struct {
	Version        int                `json:"version"`
	Binary         string             `json:"binary"`
	Helper         string             `json:"helper,omitempty"`
	Print          bool               `json:"print"`
	TrustWorkspace bool               `json:"trust_workspace"`
	Accounts       map[string]account `json:"accounts"`
}

func Store(root string) maintenance.Store {
	return maintenance.Store{Directory: filepath.Join(root, "maintenance", "claude")}
}

func decode(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrConfiguration
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrConfiguration
	}
	return nil
}

func noLinks(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		i, err := os.Lstat(p)
		if err != nil || i.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return true
}

func readPrivate(path string) ([]byte, error) {
	if !noLinks(path) {
		return nil, ErrConfiguration
	}
	i, err := os.Lstat(path)
	if err != nil || !i.Mode().IsRegular() || !privatefile.Check(path) || !owned(i) {
		return nil, ErrConfiguration
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrConfiguration
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(i, actual) {
		return nil, ErrConfiguration
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil || len(b) > maxInput {
		return nil, ErrConfiguration
	}
	return b, nil
}

func profileIdentity(path string) (identity, error) {
	b, err := readPrivate(path)
	if err != nil {
		return identity{}, err
	}
	var p struct {
		Account identity `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &p) != nil || !p.Account.valid() {
		return identity{}, ErrConfiguration
	}
	return p.Account, nil
}

func validateAccount(a account) error {
	if !a.Enabled || (a.Ownership != "current" && a.Ownership != "saved") || !noLinks(a.ConfigDir) || !privatefile.CheckDirectory(a.ConfigDir) {
		return ErrConfiguration
	}
	i, err := profileIdentity(a.Profile)
	if err != nil || i.digest() != a.Identity {
		return ErrConfiguration
	}
	return nil
}

func readConfig(root string) (config, error) {
	d, err := Store(root).Read()
	if err != nil {
		return config{}, err
	}
	var c config
	if decode(d.Adapter, &c) != nil || c.Version != 1 || len(c.Accounts) > maintenance.MaxAccounts || !filepath.IsAbs(c.Binary) || (!c.Print && !filepath.IsAbs(c.Helper)) {
		return config{}, ErrConfiguration
	}
	return c, nil
}
