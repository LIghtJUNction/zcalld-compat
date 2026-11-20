// Privacy audit persistence follows Magpie's MIT-licensed maskrec record schema.
// It deliberately stores no request body, original value or restoration mapping.
package privacy

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"github.com/LIghtJUNction/zcalld-compat/internal/redact"
)

const (
	auditDay          = "2006-01-02"
	auditRecordLimit  = 1 << 20
	auditValueLimit   = 4096
	auditAgentLimit   = 1024
	auditCounterLimit = 1 << 20
)

var errAudit = errors.New("privacy audit history is unavailable")

// Context is supplied by the gateway's existing, whitelisted request context.
// Missing/null context is supported for older native plugins.
type Context struct {
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	What    string `json:"what,omitempty"`
}

func (c Context) valid() bool {
	for _, f := range []struct {
		s string
		n int
	}{{c.Agent, 512}, {c.Session, 1024}, {c.What, 2048}} {
		if len(f.s) > f.n || !utf8.ValidString(f.s) || strings.ContainsAny(f.s, "\x00\r\n") {
			return false
		}
	}
	return true
}

type auditValue struct {
	Placeholder string `json:"p"`
	Kind        string `json:"k"`
	By          string `json:"b"`
	Head        string `json:"h,omitempty"`
	Len         int    `json:"l"`
	N           int    `json:"n"`
	Action      string `json:"a,omitempty"`
}
type auditRecord struct {
	Key         int64        `json:"key"`
	Time        time.Time    `json:"t"`
	What        string       `json:"what"`
	Agent       string       `json:"agent,omitempty"`
	Session     string       `json:"session,omitempty"`
	N           int          `json:"n"`
	Values      []auditValue `json:"values,omitempty"`
	Blocked     bool         `json:"blocked,omitempty"`
	OmittedHits int          `json:"omitted_hits,omitempty"`
}
type auditHits struct {
	values  []auditValue
	at      map[string]int
	omitted int
}

func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
func (h *auditHits) add(v redact.Hit) {
	if len(v.Placeholder) > 512 || len(v.Kind) > 128 || len(v.By) > 64 || len(v.Head) > 32 || len(v.Action) > 16 {
		h.omitted++
		return
	}
	if h.at == nil {
		h.at = make(map[string]int)
	}
	if i, ok := h.at[v.Placeholder]; ok {
		h.values[i].N++
		if h.values[i].By == "known" && v.By != "known" {
			h.values[i].By = bounded(v.By, 64)
		}
		if v.Action == "block" {
			h.values[i].Action = "block"
		}
		return
	}
	if len(h.values) >= auditValueLimit {
		h.omitted++
		return
	}
	h.at[v.Placeholder] = len(h.values)
	h.values = append(h.values, auditValue{
		Placeholder: bounded(v.Placeholder, 512), Kind: bounded(v.Kind, 128),
		By: bounded(v.By, 64), Head: bounded(v.Head, 32), Len: v.Len, N: 1, Action: bounded(v.Action, 16),
	})
}

type checkedCount struct {
	Looked int64 `json:"looked,omitempty"`
	Masked int64 `json:"masked,omitempty"`
	Whole  int64 `json:"whole,omitempty"`
}

func sumCount(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
func (c checkedCount) add(d checkedCount) checkedCount {
	return checkedCount{sumCount(c.Looked, d.Looked), sumCount(c.Masked, d.Masked), sumCount(c.Whole, d.Whole)}
}

type auditStore struct {
	mu            sync.Mutex
	dir           string
	now           func() time.Time
	every         time.Duration
	lockWait      time.Duration
	keepDays      int
	keepBytes     int64
	pending       map[string]map[string]checkedCount
	timer         *time.Timer
	closed        bool
	lastError     error
	pruned        time.Time
	lastDir       os.FileInfo
	desktopInbox  string
	firstVerified map[string]firstVerification
}

func newAuditStore(dir string) *auditStore {
	return &auditStore{dir: dir, now: time.Now, every: 5 * time.Second, lockWait: 250 * time.Millisecond,
		keepDays: 30, keepBytes: 100 << 20, pending: map[string]map[string]checkedCount{}}
}
func (a *auditStore) note(c Context, h auditHits, n int, blocked bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errAudit
	}
	now := a.now()
	changed := n > 0 || len(h.values) > 0 || h.omitted > 0
	if changed {
		what := c.What
		if what == "" {
			what = "generation"
		}
		if blocked {
			what = "blocked"
		}
		r := auditRecord{Time: now, What: what, Agent: c.Agent, Session: c.Session, N: n,
			Values: h.values, Blocked: blocked, OmittedHits: h.omitted}
		var event *firstEvent
		var notificationErr error
		if err := a.withLock(func(lock *os.File) error {
			var first *firstIndex
			if a.desktopInbox != "" {
				if a.firstVerified == nil {
					a.firstVerified = make(map[string]firstVerification)
				}
				first, notificationErr = prepareFirst(a.dir, a.firstVerified)
			}
			if err := a.append(lock, &r, a.desktopInbox != ""); err != nil {
				return err
			}
			if first != nil && notificationErr == nil {
				var counts map[string]int
				counts, notificationErr = first.record(a.dir, r)
				if notificationErr == nil {
					event, notificationErr = makeFirstEvent(c.Agent, counts)
				}
			}
			if a.pruned.IsZero() || now.Sub(a.pruned) >= time.Hour {
				if err := a.prune(now); err != nil {
					return err
				}
				a.pruned = now
			}
			return nil
		}); err != nil {
			a.lastError = errAudit
		} else if notificationErr != nil || publishFirst(a.desktopInbox, event) != nil {
			a.lastError = errFirst
		}
	}
	day := now.Local().Format(auditDay)
	oldest := now.Local().AddDate(0, 0, -a.keepDays).Format(auditDay)
	for d := range a.pending {
		if d < oldest {
			delete(a.pending, d)
		}
	}
	if a.pending[day] == nil {
		a.pending[day] = map[string]checkedCount{}
	}
	agent := c.Agent
	if agent == "" {
		agent = "other"
	}
	if _, ok := a.pending[day][agent]; !ok && len(a.pending[day]) >= auditAgentLimit {
		agent = "other"
	}
	d := checkedCount{Looked: 1}
	if changed {
		d.Masked = 1
	}
	a.pending[day][agent] = a.pending[day][agent].add(d)
	a.schedule()
	return a.lastError
}
func (a *auditStore) schedule() {
	if a.timer != nil || a.closed || len(a.pending) == 0 {
		return
	}
	a.timer = time.AfterFunc(a.every, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.timer = nil
		if a.closed {
			return
		}
		a.flushLocked()
		if len(a.pending) > 0 {
			a.schedule()
		}
	})
}
func (a *auditStore) flushLocked() error {
	if len(a.pending) == 0 {
		return a.lastError
	}
	err := a.withLock(func(_ *os.File) error {
		days := make([]string, 0, len(a.pending))
		for day := range a.pending {
			days = append(days, day)
		}
		sort.Strings(days)
		for _, day := range days {
			counts, err := a.readCounts(day)
			if err != nil {
				return err
			}
			for agent, delta := range a.pending[day] {
				if _, ok := counts[agent]; !ok && len(counts) >= auditAgentLimit {
					agent = "other"
				}
				counts[agent] = counts[agent].add(delta)
			}
			committed, err := a.writeCounts(day, counts)
			if committed {
				delete(a.pending, day)
			}
			if err != nil {
				return err
			}
		}
		return a.pruneChecked(a.now())
	})
	if err != nil {
		a.lastError = errAudit
	} else {
		a.lastError = nil
	}
	return a.lastError
}
func (a *auditStore) flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flushLocked()
}

// Close stops the timer and flushes counters on graceful stdio EOF. Calling it
// again retries a failed flush, without appending any detection record twice.
func (a *auditStore) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	return a.flushLocked()
}
func (a *auditStore) withLock(fn func(*os.File) error) error {
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return errAudit
	}
	info, err := os.Lstat(a.dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errAudit
	}
	if privatefile.Protect(a.dir, 0700) != nil {
		return errAudit
	}
	lock, err := openAuditFile(auditLockPath(a.dir), os.O_RDWR)
	if err != nil {
		return errAudit
	}
	defer lock.Close()
	unlock, err := auditLock(lock, a.lockWait)
	if err != nil {
		return errAudit
	}
	defer unlock()
	lockInfo, statErr := lock.Stat()
	if statErr != nil || (lockInfo.Size() != 0 && lockInfo.Size() != 8) {
		return errAudit
	}
	if !sameAuditLocation(a.dir, info, lock) {
		return errAudit
	}
	return fn(lock)
}

func auditLockPath(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".zcalld-audit.lock")
}

// A cutover can exchange directories while this process waits for a lock.
// Never use an old inode lock to protect writes addressed through a new path.
func sameAuditLocation(dir string, before os.FileInfo, lock *os.File) bool {
	after, e1 := os.Lstat(dir)
	named, e2 := os.Lstat(auditLockPath(dir))
	held, e3 := lock.Stat()
	return e1 == nil && e2 == nil && e3 == nil && after.IsDir() && named.Mode().IsRegular() && os.SameFile(before, after) && os.SameFile(named, held)
}

// openAuditFile checks identity and type before any content write. The audit
// directory is private; existing symlinks and non-regular leaves are rejected.
func openAuditFile(path string, flags int) (*os.File, error) {
	info, err := os.Lstat(path)
	var f *os.File
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return openAuditFile(path, flags)
		}
	} else if err == nil && info.Mode().IsRegular() {
		f, err = os.OpenFile(path, flags, 0600)
	} else {
		return nil, errAudit
	}
	if err != nil {
		return nil, errAudit
	}
	opened, e1 := f.Stat()
	current, e2 := os.Lstat(path)
	if e1 != nil || e2 != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		privatefile.Protect(path, 0600) != nil || !privatefile.Check(path) {
		f.Close()
		return nil, errAudit
	}
	return f, nil
}
func (a *auditStore) append(lock *os.File, r *auditRecord, durable bool) error {
	var key [8]byte
	n, err := lock.ReadAt(key[:], 0)
	if err != nil && !(err == io.EOF && n == 0) {
		return errAudit
	}
	last := int64(binary.BigEndian.Uint64(key[:]))
	info, err := os.Lstat(a.dir)
	if err != nil || !info.IsDir() {
		return errAudit
	}
	if n == 0 || a.lastDir == nil || !os.SameFile(a.lastDir, info) {
		if err := a.prune(r.Time); err != nil {
			return err
		}
		historyLast, err := a.historyLastKey()
		if err != nil {
			return err
		}
		last = max(last, historyLast)
		a.lastDir = info
	}
	r.Key = r.Time.UnixNano()
	if r.Key <= last {
		if last == math.MaxInt64 {
			return errAudit
		}
		r.Key = last + 1
	}
	binary.BigEndian.PutUint64(key[:], uint64(r.Key))
	if n, err := lock.WriteAt(key[:], 0); err != nil || n != len(key) {
		return errAudit
	}
	data, err := json.Marshal(r)
	for err == nil && len(data) > auditRecordLimit && len(r.Values) > 0 {
		cut := len(r.Values) / 2
		for _, v := range r.Values[cut:] {
			r.OmittedHits += v.N
		}
		r.Values = r.Values[:cut]
		data, err = json.Marshal(r)
	}
	if err != nil || len(data) > auditRecordLimit {
		return errAudit
	}
	file, err := openAuditFile(filepath.Join(a.dir, r.Time.Local().Format(auditDay)+".jsonl"), os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err = file.Write(data)
	if err == nil && durable {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || n != len(data) || closeErr != nil {
		return errAudit
	}
	if durable && privatefile.SyncDir(a.dir) != nil {
		return errAudit
	}
	return nil
}

// Rebuild when a cutover copies history without this implementation's lock.
// Read only bounded daily metadata, never a request body or restoration map.
func (a *auditStore) historyLastKey() (int64, error) {
	files, err := a.dailyFiles("", ".jsonl")
	if err != nil {
		return 0, err
	}
	var total, last int64
	for _, entry := range files {
		total += entry.size
		if total > 128<<20 {
			return 0, errAudit
		}
		f, err := os.Open(entry.path)
		if err != nil {
			return 0, errAudit
		}
		info, err := f.Stat()
		original, e := os.Lstat(entry.path)
		if err != nil || e != nil || !original.Mode().IsRegular() || !os.SameFile(info, original) || !privatefile.Check(entry.path) {
			f.Close()
			return 0, errAudit
		}
		scan := bufio.NewScanner(io.LimitReader(f, entry.size+1))
		scan.Buffer(make([]byte, 65536), auditRecordLimit+1)
		for scan.Scan() {
			var old struct {
				Key int64 `json:"key"`
			}
			if json.Unmarshal(scan.Bytes(), &old) == nil && old.Key > last {
				last = old.Key
			}
		}
		scanErr := scan.Err()
		closeErr := f.Close()
		if scanErr != nil || closeErr != nil {
			return 0, errAudit
		}
	}
	return last, nil
}
func (a *auditStore) readCounts(day string) (map[string]checkedCount, error) {
	path := filepath.Join(a.dir, "checked-"+day+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]checkedCount{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > auditCounterLimit || !privatefile.Check(path) {
		return nil, errAudit
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errAudit
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errAudit
	}
	data, err := io.ReadAll(io.LimitReader(f, auditCounterLimit+1))
	var counts map[string]checkedCount
	if err != nil || len(data) > auditCounterLimit || json.Unmarshal(data, &counts) != nil || counts == nil || len(counts) > auditAgentLimit+1 {
		return nil, errAudit
	}
	for agent, c := range counts {
		if len(agent) > 512 || c.Looked < 0 || c.Masked < 0 || c.Whole < 0 {
			return nil, errAudit
		}
	}
	return counts, nil
}
func (a *auditStore) writeCounts(day string, counts map[string]checkedCount) (bool, error) {
	data, err := json.Marshal(counts)
	if err != nil || len(data) > auditCounterLimit {
		return false, errAudit
	}
	f, err := os.CreateTemp(a.dir, ".checked-*")
	if err != nil {
		return false, errAudit
	}
	path := f.Name()
	defer os.Remove(path)
	if privatefile.Protect(path, 0600) != nil {
		f.Close()
		return false, errAudit
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return false, errAudit
	}
	dest := filepath.Join(a.dir, "checked-"+day+".json")
	if info, e := os.Lstat(dest); e == nil && !info.Mode().IsRegular() {
		return false, errAudit
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, errAudit
	}
	if os.Rename(path, dest) != nil {
		return false, errAudit
	}
	if privatefile.SyncDir(a.dir) != nil {
		return true, errAudit
	}
	return true, nil
}

type auditFile struct {
	day, path string
	size      int64
}

func (a *auditStore) dailyFiles(prefix, suffix string) ([]auditFile, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, errAudit
	}
	var out []auditFile
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
		if len(day) != 10 {
			continue
		}
		if _, err := time.Parse(auditDay, day); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil, errAudit
		}
		out = append(out, auditFile{day, filepath.Join(a.dir, name), info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].day < out[j].day })
	return out, nil
}
func (a *auditStore) prune(now time.Time) error {
	files, err := a.dailyFiles("", ".jsonl")
	if err != nil {
		return err
	}
	oldest := now.Local().AddDate(0, 0, -a.keepDays).Format(auditDay)
	var total int64
	for _, f := range files {
		total += f.size
	}
	for _, f := range files {
		if f.day >= oldest && total <= a.keepBytes {
			break
		}
		if os.Remove(f.path) != nil {
			return errAudit
		}
		total -= f.size
	}
	return a.pruneChecked(now)
}
func (a *auditStore) pruneChecked(now time.Time) error {
	files, err := a.dailyFiles("checked-", ".json")
	if err != nil {
		return err
	}
	oldest := now.Local().AddDate(0, 0, -a.keepDays).Format(auditDay)
	for _, f := range files {
		if f.day >= oldest {
			break
		}
		if os.Remove(f.path) != nil {
			return errAudit
		}
	}
	return nil
}

// Empty activation probes are not turns. Only content-bearing request fields
// participate; model IDs, roles, and generation options alone do not count.
func hasAuditContent(body []byte) bool {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, key := range []string{"messages", "contents", "input", "prompt", "instructions", "system"} {
		var v any
		if raw, ok := request[key]; ok && json.Unmarshal(raw, &v) == nil && auditContent(v, 0) {
			return true
		}
	}
	return false
}
func auditContent(v any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		for _, child := range x {
			if auditContent(child, depth+1) {
				return true
			}
		}
	case map[string]any:
		for key, child := range x {
			switch key {
			case "role", "type", "id", "name", "model", "index", "status":
				continue
			}
			if auditContent(child, depth+1) {
				return true
			}
		}
	}
	return false
}
