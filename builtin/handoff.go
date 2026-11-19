package builtin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/community"
)

// OwnerManifest is a private, explicit inventory of the native sessions whose
// refresh ownership is being handed over. Future Codex sessions must already be
// configured to use the gateway without native OAuth before Handoff is called.
// It deliberately contains no command lines, email addresses, or token values.
type OwnerManifest struct {
	Version        int            `json:"version"`
	Platform       string         `json:"platform"`
	BootID         string         `json:"boot_id"`
	PIDNamespace   string         `json:"pid_namespace"`
	SourceAuthFile string         `json:"source_auth_file"`
	AccountHash    string         `json:"account_hash"`
	CapturedAt     string         `json:"captured_at"`
	Holders        []OwnerProcess `json:"holders"`
}

type OwnerProcess struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}

// Linux birth times are read from procfs rather than inferred from a process
// name. A reused PID belongs to a different process and is not the old holder.
func linuxProcess(pid int) (uint64, bool, error) {
	if pid <= 0 {
		return 0, false, errors.New("owner PID must be positive")
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, errors.New("cannot verify a native credential holder")
	}
	// comm is parenthesized and can contain spaces and closing parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, false, errors.New("invalid native process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return 0, false, errors.New("incomplete native process identity")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, false, errors.New("invalid native process birth time")
	}
	return start, fields[0] != "Z" && fields[0] != "X", nil
}

func ownerEnvironment() (boot, namespace string, err error) {
	if runtime.GOOS != "linux" {
		return "", "", errors.New("native owner handoff verification is currently available on Linux; keep external ownership on this platform")
	}
	data, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", "", errors.New("cannot verify native owner boot identity")
	}
	boot = strings.TrimSpace(string(data))
	if len(boot) != 36 {
		return "", "", errors.New("invalid boot identity")
	}
	namespace, e = os.Readlink("/proc/self/ns/pid")
	if e != nil {
		return "", "", errors.New("cannot verify native owner PID namespace")
	}
	return boot, namespace, nil
}

func (m OwnerManifest) validate() error {
	if m.Version != 1 || m.Platform != "linux" || len(m.BootID) != 36 || m.PIDNamespace == "" || !filepath.IsAbs(m.SourceAuthFile) || len(m.AccountHash) != 64 || len(m.Holders) == 0 || len(m.Holders) > 1024 {
		return errors.New("invalid native owner manifest")
	}
	if _, err := hex.DecodeString(m.AccountHash); err != nil {
		return errors.New("invalid native owner account fingerprint")
	}
	if _, err := time.Parse(time.RFC3339, m.CapturedAt); err != nil {
		return errors.New("invalid native owner capture time")
	}
	seen := map[int]bool{}
	for _, holder := range m.Holders {
		if holder.PID <= 0 || holder.StartTicks == 0 || seen[holder.PID] {
			return errors.New("invalid or duplicate native credential holder")
		}
		seen[holder.PID] = true
	}
	return nil
}

func (m OwnerManifest) allExited() error {
	boot, namespace, err := ownerEnvironment()
	if err != nil {
		return err
	}
	if namespace != m.PIDNamespace {
		return errors.New("owner manifest belongs to a different PID namespace")
	}
	if boot != m.BootID {
		return nil
	} // A reboot ended every registered old session.
	active := 0
	for _, holder := range m.Holders {
		start, alive, err := linuxProcess(holder.PID)
		if err != nil {
			return err
		}
		if alive && start == holder.StartTicks {
			active++
		}
	}
	if active > 0 {
		return fmt.Errorf("%d registered native credential holder(s) are still running; external ownership is retained", active)
	}
	return nil
}

// renewalFence uses the host's own per-account lock name and owner format. It
// prevents an external read-copy refresh already in flight from writing its old
// ownership fields after the handoff. No lock is stolen or removed on failure.
func renewalFence(authPath, key string) (func(), error) {
	name := authPath + ".renew-" + hash(key)[:16] + ".lock"
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("account renewal is active; retry the explicit handoff after it finishes")
	}
	host, err := os.Hostname()
	if err != nil {
		f.Close()
		os.Remove(name)
		return nil, err
	}
	ns, _ := os.Readlink("/proc/self/ns/pid")
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		f.Close()
		os.Remove(name)
		return nil, err
	}
	data, err := json.Marshal(object{"pid": os.Getpid(), "host": strings.TrimSpace(host + " " + ns), "at": time.Now().UnixMilli(), "n": hex.EncodeToString(nonce[:])})
	if err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(name)
		return nil, err
	}
	return func() {
		current, e := os.ReadFile(name)
		if e == nil && string(current) == string(data) {
			_ = os.Remove(name)
		}
	}, nil
}

// Handoff claims refresh ownership only after every explicitly recorded old
// process has exited. It reads the latest source token without network traffic,
// verifies the account, and refreshes the rollback fingerprint. It never kills
// a process and is never called automatically in the background.
func Handoff(root, key, manifestPath string) (map[string]any, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(manifestPath) || !strings.HasPrefix(key, "builtin:codex#") {
		return nil, errors.New("handoff requires an absolute private manifest and a scoped builtin Codex account")
	}
	st, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return nil, errors.New("owner manifest must be a private regular file (0600)")
	}
	var owners OwnerManifest
	if err = read(manifestPath, &owners); err != nil {
		return nil, err
	}
	if err = owners.validate(); err != nil {
		return nil, err
	}
	if err = owners.allExited(); err != nil {
		return nil, err
	}
	unlock, err := community.Lock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	authPath := filepath.Join(root, "plugin-auth.json")
	release, err := claimAuth(authPath)
	if err != nil {
		return nil, err
	}
	defer release()
	fence, err := renewalFence(authPath, key)
	if err != nil {
		return nil, err
	}
	defer fence()
	var auth object
	if err = read(authPath, &auth); err != nil {
		return nil, err
	}
	current := obj(auth[key])
	if current == nil || str(current["refreshOwner"]) != "external" {
		return nil, errors.New("Codex account is not externally managed")
	}
	path, err := canonical(str(current["sourceAuthFile"]))
	if err != nil {
		return nil, errors.New("external Codex source is unavailable")
	}
	if path != owners.SourceAuthFile || hash(str(current["accountId"])) != owners.AccountHash {
		return nil, errors.New("owner manifest does not match this Codex account and source")
	}
	var mapping Mapping
	mapPath := filepath.Join(root, "builtin-auth-map.json")
	if err = read(mapPath, &mapping); err != nil {
		return nil, err
	}
	if mapping.Version != 1 {
		return nil, errors.New("unsupported builtin migration map")
	}
	var account *Account
	for i := range mapping.Accounts {
		if mapping.Accounts[i].Key == key && mapping.Accounts[i].Agent == "codex" {
			account = &mapping.Accounts[i]
			break
		}
	}
	if account == nil {
		return nil, errors.New("Codex account has no reversible source mapping")
	}
	var source *Source
	for i := range account.Sources {
		if account.Sources[i].Kind == "codex" && account.Sources[i].Path == path {
			source = &account.Sources[i]
			break
		}
	}
	if source == nil {
		return nil, errors.New("Codex source does not match its private migration map")
	}
	var raw object
	if err = read(path, &raw); err != nil {
		return nil, err
	}
	fresh := normalized("codex", raw)
	if fresh == nil || str(fresh["accountId"]) == "" || hash(str(fresh["accountId"])) != owners.AccountHash {
		return nil, errors.New("external Codex account identity changed")
	}
	if !validToken(str(fresh["refresh"])) {
		return nil, errors.New("external Codex source has no valid refresh token")
	}
	before := digest(raw)
	// Recheck immediately before persistence. The caller fences future native
	// sessions through gateway configuration; this manifest covers old sessions.
	if err = owners.allExited(); err != nil {
		return nil, err
	}
	var reread object
	if err = read(path, &reread); err != nil {
		return nil, err
	}
	if digest(reread) != before {
		return nil, errors.New("external Codex credentials changed during handoff; retry with the current source")
	}
	for field, value := range fresh {
		current[field] = value
	}
	current["refreshOwner"] = "host"
	delete(current, "sourceAuthFile")
	source.SecretHash = tokenHash(fresh)
	account.ImportedHash = digest(current)
	// Persist the rollback map first: interruption before auth changes still
	// leaves the account external. After auth changes both documents are ready.
	if err = save(mapPath, mapping); err != nil {
		return nil, err
	}
	if err = save(authPath, auth); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "refresh_owner": "host", "verified_exited_holders": len(owners.Holders), "network_requests": 0}, nil
}
