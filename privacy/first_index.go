package privacy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

// Only this optional index stores first-seen state. Buckets contain sorted
// SHA-256 digests, never values, placeholders, heads, sessions or request text.
// The manifest is the commit point: immutable bucket versions let a crash leave
// harmless unreferenced files, instead of an ambiguous partly updated index.
const (
	firstBucketBytes  = 64 << 10
	firstStateBytes   = 64 << 10
	firstHistoryMax   = 128 << 20
	firstHistoryDays  = 128
	firstBatchSize    = 4096
	firstDirectoryMax = 1024
)

var errFirst = errors.New("privacy first-hit notification is unavailable")

type firstBucket struct {
	Hash  string `json:"hash"`
	Count int    `json:"count"`
}
type firstStamp struct {
	Size     int64 `json:"size"`
	Modified int64 `json:"modified"`
}
type firstState struct {
	Version int                    `json:"version"`
	Ready   bool                   `json:"ready"`
	Buckets map[string]firstBucket `json:"buckets"`
	History map[string]firstStamp  `json:"history"`
}
type firstHistoryFile struct {
	path  string
	stamp firstStamp
	info  os.FileInfo
}
type firstVerification struct {
	Info     os.FileInfo
	Size     int64
	Modified int64
}

type firstIndex struct {
	dir      string
	info     os.FileInfo
	state    firstState
	original map[string]firstBucket
}

// The sibling is protected by the existing stable audit lock. It stays outside
// the interchangeable history payload, so a rollback does not discard dedup.
func firstIndexPath(history string) string {
	return filepath.Join(filepath.Dir(history), "."+filepath.Base(history)+".zcalld-first")
}

func firstHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func firstDigest(p string) [32]byte {
	return sha256.Sum256(append([]byte("zcalld/privacy-first/v1\x00"), []byte(p)...))
}

func firstRead(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit || !firstPrivateNode(path, before) {
		return nil, errFirst
	}
	f, err := firstOpen(path)
	if err != nil {
		return nil, errFirst
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) {
		return nil, errFirst
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, statErr := os.Lstat(path)
	if err != nil || int64(len(data)) > limit || int64(len(data)) != before.Size() ||
		statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, errFirst
	}
	return data, nil
}

func (i *firstIndex) sameDirectory() bool {
	info, err := os.Lstat(i.dir)
	return err == nil && info.IsDir() && os.SameFile(info, i.info) && firstPrivateNode(i.dir, info)
}

func firstBucketName(prefix string, b firstBucket) string {
	return prefix + "-" + b.Hash + ".bin"
}

func (i *firstIndex) writeAtomic(name string, data []byte) error {
	if !i.sameDirectory() {
		return errFirst
	}
	dest := filepath.Join(i.dir, name)
	if old, err := os.Lstat(dest); err == nil {
		if !old.Mode().IsRegular() || !firstPrivateNode(dest, old) {
			return errFirst
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errFirst
	}
	f, err := os.CreateTemp(i.dir, ".first-write-")
	if err != nil {
		return errFirst
	}
	path := f.Name()
	defer os.Remove(path)
	if err = privatefile.Protect(path, 0600); err == nil {
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || !i.sameDirectory() {
		return errFirst
	}
	if os.Rename(path, dest) != nil {
		return errFirst
	}
	return nil
}

func (i *firstIndex) commit(history map[string]firstStamp, ready bool) error {
	i.state.History, i.state.Ready = history, ready
	data, err := json.Marshal(i.state)
	if err != nil || len(data) > firstStateBytes {
		return errFirst
	}
	if i.writeAtomic("state.json", data) != nil || privatefile.SyncDir(i.dir) != nil {
		return errFirst
	}
	i.original = make(map[string]firstBucket, len(i.state.Buckets))
	for k, b := range i.state.Buckets {
		i.original[k] = b
	}
	return i.collect()
}

// Garbage collection accepts only our exact filenames and private regular
// nodes. It never recursively removes a directory or follows a symlink.
func (i *firstIndex) collect() error {
	if !i.sameDirectory() {
		return errFirst
	}
	f, err := os.Open(i.dir)
	if err != nil {
		return errFirst
	}
	entries, err := f.ReadDir(firstDirectoryMax + 1)
	f.Close()
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > firstDirectoryMax {
		return errFirst
	}
	kept := map[string]bool{"state.json": true}
	for prefix, b := range i.state.Buckets {
		kept[firstBucketName(prefix, b)] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(i.dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !firstPrivateNode(path, info) {
			return errFirst
		}
		if kept[name] {
			continue
		}
		bucket := len(name) == 71 && name[2] == '-' && strings.HasSuffix(name, ".bin") &&
			firstHex(name[:2], 2) && firstHex(name[3:67], 64)
		staged := strings.HasPrefix(name, ".first-write-") && len(name) < 96
		if !bucket && !staged {
			return errFirst
		}
		if os.Remove(path) != nil {
			return errFirst
		}
	}
	return nil
}

func openFirstIndex(history string) (*firstIndex, error) {
	dir := firstIndexPath(history)
	parent, err := os.Lstat(filepath.Dir(dir))
	if err != nil || !parent.IsDir() || !firstPrivateNode(filepath.Dir(dir), parent) {
		return nil, errFirst
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if os.Mkdir(dir, 0700) != nil || privatefile.Protect(dir, 0700) != nil ||
			privatefile.SyncDir(filepath.Dir(dir)) != nil {
			return nil, errFirst
		}
		info, err = os.Lstat(dir)
	}
	if err != nil || !info.IsDir() || !firstPrivateNode(dir, info) {
		return nil, errFirst
	}
	i := &firstIndex{dir: dir, info: info}
	path := filepath.Join(dir, "state.json")
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		// An empty newly created directory is recoverable. Losing the manifest
		// of a nonempty index is corruption, never permission to announce again.
		f, e := os.Open(dir)
		if e != nil {
			return nil, errFirst
		}
		entries, e := f.ReadDir(1)
		f.Close()
		if (e != nil && !errors.Is(e, io.EOF)) || len(entries) > 0 {
			return nil, errFirst
		}
		i.state = firstState{Version: 1, Buckets: map[string]firstBucket{}, History: map[string]firstStamp{}}
		if i.commit(i.state.History, false) != nil {
			return nil, errFirst
		}
	} else {
		data, e := firstRead(path, firstStateBytes)
		if e != nil {
			return nil, e
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if dec.Decode(&i.state) != nil || i.state.Version != 1 || i.state.Buckets == nil || i.state.History == nil ||
			len(i.state.Buckets) > 256 || len(i.state.History) > firstHistoryDays {
			return nil, errFirst
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return nil, errFirst
		}
		for prefix, b := range i.state.Buckets {
			if !firstHex(prefix, 2) || !firstHex(b.Hash, 64) || b.Count < 1 || b.Count > firstBucketBytes/32 {
				return nil, errFirst
			}
		}
		for name, s := range i.state.History {
			if !firstDayName(name) || s.Size < 0 || s.Size > firstHistoryMax {
				return nil, errFirst
			}
		}
		i.original = make(map[string]firstBucket, len(i.state.Buckets))
		for k, b := range i.state.Buckets {
			i.original[k] = b
		}
		if i.collect() != nil {
			return nil, errFirst
		}
	}
	return i, nil
}

func firstDayName(name string) bool {
	if len(name) != 16 || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	_, err := time.Parse(auditDay, strings.TrimSuffix(name, ".jsonl"))
	return err == nil
}

func firstHistoryFiles(dir string) (map[string]firstHistoryFile, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, errFirst
	}
	entries, err := f.ReadDir(firstHistoryDays*3 + 1)
	f.Close()
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > firstHistoryDays*3 {
		return nil, errFirst
	}
	files := make(map[string]firstHistoryFile)
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !firstDayName(name) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !firstPrivateNode(path, info) {
			return nil, errFirst
		}
		total += info.Size()
		if info.Size() < 0 || total > firstHistoryMax || len(files) >= firstHistoryDays {
			return nil, errFirst
		}
		files[name] = firstHistoryFile{path, firstStamp{info.Size(), info.ModTime().UnixNano()}, info}
	}
	return files, nil
}

func firstStamps(files map[string]firstHistoryFile) map[string]firstStamp {
	out := make(map[string]firstStamp, len(files))
	for name, f := range files {
		out[name] = f.stamp
	}
	return out
}

func (i *firstIndex) bucket(prefix string) ([][32]byte, error) {
	meta, ok := i.state.Buckets[prefix]
	if !ok {
		return nil, nil
	}
	data, err := firstRead(filepath.Join(i.dir, firstBucketName(prefix, meta)), firstBucketBytes)
	if err != nil || len(data) != meta.Count*32 {
		return nil, errFirst
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != meta.Hash {
		return nil, errFirst
	}
	out := make([][32]byte, meta.Count)
	for n := range out {
		copy(out[n][:], data[n*32:(n+1)*32])
		if fmt.Sprintf("%02x", out[n][0]) != prefix || (n > 0 && bytes.Compare(out[n-1][:], out[n][:]) >= 0) {
			return nil, errFirst
		}
	}
	return out, nil
}

// A batch is at most 4096 digests. Only one 64-KiB committed bucket is loaded
// while merging; a retained transaction holds at most one new version per
// bucket in addition to the manifest's old versions.
func (i *firstIndex) add(batch map[[32]byte]struct{}) (map[[32]byte]bool, error) {
	groups := map[byte][][32]byte{}
	for d := range batch {
		groups[d[0]] = append(groups[d[0]], d)
	}
	fresh := make(map[[32]byte]bool)
	for b, add := range groups {
		prefix := fmt.Sprintf("%02x", b)
		old, err := i.bucket(prefix)
		if err != nil {
			return nil, err
		}
		set := make(map[[32]byte]bool, len(old)+len(add))
		for _, d := range old {
			set[d] = true
		}
		changed := false
		for _, d := range add {
			if !set[d] {
				set[d] = true
				fresh[d] = true
				changed = true
			}
		}
		if len(set) > firstBucketBytes/32 {
			return nil, errFirst
		}
		if !changed {
			continue
		}
		all := make([][32]byte, 0, len(set))
		for d := range set {
			all = append(all, d)
		}
		sort.Slice(all, func(a, b int) bool { return bytes.Compare(all[a][:], all[b][:]) < 0 })
		data := make([]byte, 0, len(all)*32)
		for _, d := range all {
			data = append(data, d[:]...)
		}
		h := sha256.Sum256(data)
		meta := firstBucket{hex.EncodeToString(h[:]), len(all)}
		name := firstBucketName(prefix, meta)
		if existing, e := os.Lstat(filepath.Join(i.dir, name)); e == nil {
			known, e := firstRead(filepath.Join(i.dir, name), firstBucketBytes)
			if e != nil || !existing.Mode().IsRegular() || !bytes.Equal(known, data) {
				return nil, errFirst
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, errFirst
		} else if i.writeAtomic(name, data) != nil {
			return nil, errFirst
		}
		prev, ok := i.state.Buckets[prefix]
		i.state.Buckets[prefix] = meta
		if original := i.original[prefix]; ok && prev.Hash != original.Hash && prev.Hash != meta.Hash {
			if os.Remove(filepath.Join(i.dir, firstBucketName(prefix, prev))) != nil {
				return nil, errFirst
			}
		}
	}
	return fresh, nil
}

func (i *firstIndex) seedFile(entry firstHistoryFile, batch map[[32]byte]struct{}) error {
	f, err := firstOpen(entry.path)
	if err != nil {
		return errFirst
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil || !os.SameFile(entry.info, held) {
		return errFirst
	}
	if held.Size() > 0 {
		var end [1]byte
		if n, e := f.ReadAt(end[:], held.Size()-1); e != nil || n != 1 || end[0] != '\n' {
			return errFirst
		}
	}
	scan := bufio.NewScanner(io.LimitReader(f, entry.stamp.Size))
	scan.Buffer(make([]byte, 64<<10), auditRecordLimit+1)
	for scan.Scan() {
		var record struct {
			Values []auditValue `json:"values"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil || len(record.Values) > auditValueLimit {
			return errFirst
		}
		for _, v := range record.Values {
			if v.Placeholder == "" || len(v.Placeholder) > 512 || !utf8.ValidString(v.Placeholder) {
				return errFirst
			}
			batch[firstDigest(v.Placeholder)] = struct{}{}
			if len(batch) >= firstBatchSize {
				if _, err = i.add(batch); err != nil {
					return err
				}
				clear(batch)
			}
		}
	}
	after, statErr := os.Lstat(entry.path)
	if scan.Err() != nil || statErr != nil || !os.SameFile(entry.info, after) ||
		entry.stamp.Size != after.Size() || entry.stamp.Modified != after.ModTime().UnixNano() {
		return errFirst
	}
	return nil
}

// Call before appending the current observation, under the audit lock. An
// interrupted previous write is present in history but absent from the cursor;
// silently indexing that tail prevents a restart from announcing it as new.
func prepareFirst(history string, verified map[string]firstVerification) (*firstIndex, error) {
	i, err := openFirstIndex(history)
	if err != nil {
		return nil, err
	}
	if err := i.verify(verified); err != nil {
		return nil, err
	}
	files, err := firstHistoryFiles(history)
	if err != nil {
		return nil, err
	}
	batch := make(map[[32]byte]struct{})
	changed := !i.state.Ready || len(files) != len(i.state.History)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := files[name]
		if old, ok := i.state.History[name]; i.state.Ready && ok && old == entry.stamp {
			continue
		}
		changed = true
		if err = i.seedFile(entry, batch); err != nil {
			return nil, err
		}
	}
	if len(batch) > 0 {
		if _, err = i.add(batch); err != nil {
			return nil, err
		}
	}
	if changed {
		// Do not commit a cursor if a noncooperating writer changed any file
		// during the bounded seed. A later request may safely try again.
		after, e := firstHistoryFiles(history)
		if e != nil || !sameFirstFiles(files, after) {
			return nil, errFirst
		}
		if i.commit(firstStamps(files), true) != nil {
			return nil, errFirst
		}
	}
	return i, nil
}

func sameFirstFiles(a, b map[string]firstHistoryFile) bool {
	if len(a) != len(b) {
		return false
	}
	for name, f := range a {
		other, ok := b[name]
		if !ok || f.stamp != other.stamp || !os.SameFile(f.info, other.info) {
			return false
		}
	}
	return true
}

// The caller has durably appended r. Commit every recorded placeholder,
// including warn-only observations; only the fresh mask/block subset can alert.
func (i *firstIndex) record(history string, r auditRecord) (map[string]int, error) {
	batch := make(map[[32]byte]struct{}, len(r.Values))
	for _, v := range r.Values {
		if v.Placeholder == "" || len(v.Placeholder) > 512 {
			return nil, errFirst
		}
		batch[firstDigest(v.Placeholder)] = struct{}{}
	}
	fresh, err := i.add(batch)
	if err != nil {
		return nil, err
	}
	files, err := firstHistoryFiles(history)
	if err != nil {
		return nil, err
	}
	today := r.Time.Local().Format(auditDay) + ".jsonl"
	encoded, e := json.Marshal(r)
	current, ok := files[today]
	before := i.state.History[today]
	if e != nil || !ok || current.stamp.Size != before.Size+int64(len(encoded)+1) {
		return nil, errFirst
	}
	for name, file := range files {
		if name == today {
			continue
		}
		if old, ok := i.state.History[name]; !ok || old != file.stamp {
			return nil, errFirst
		}
	}
	// A failed fsync or bucket/manifest commit never reaches publication.
	if i.commit(firstStamps(files), true) != nil {
		return nil, errFirst
	}
	counts := make(map[string]int)
	seen := make(map[[32]byte]bool)
	for _, v := range r.Values {
		d := firstDigest(v.Placeholder)
		if !fresh[d] || seen[d] || v.Action == "warn" {
			continue
		}
		seen[d] = true
		counts[firstKind(v.Kind)]++
	}
	if r.OmittedHits > 0 {
		return map[string]int{}, nil
	}
	return counts, nil
}

// Cache only verified immutable-file identities and timestamps. A fresh worker
// verifies every committed bucket once; ordinary requests hash only changed
// buckets. A corrupt unrelated bucket therefore suppresses this notification,
// too, without rereading the whole index on each request.
func (i *firstIndex) verify(cache map[string]firstVerification) error {
	if len(cache) > 512 {
		clear(cache)
	}
	for prefix, meta := range i.state.Buckets {
		name := firstBucketName(prefix, meta)
		path := filepath.Join(i.dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !firstPrivateNode(path, info) || info.Size() != int64(meta.Count*32) {
			return errFirst
		}
		old, ok := cache[name]
		if ok && old.Size == info.Size() && old.Modified == info.ModTime().UnixNano() && os.SameFile(old.Info, info) {
			continue
		}
		if _, err = i.bucket(prefix); err != nil {
			return errFirst
		}
		cache[name] = firstVerification{info, info.Size(), info.ModTime().UnixNano()}
	}
	return nil
}
