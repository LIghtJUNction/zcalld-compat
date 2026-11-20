// Package desktop publishes optional local desktop events without a resident
// process or any network access. A failed notification must not fail its caller's
// primary operation. The consumer creates and owns the configured private inbox.
package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

const MaxEventBytes = 16 << 10
const MaxEvents = 128
const MaxInboxBytes = 2 << 20

var ErrBusy = errors.New("desktop inbox publisher is busy")
var ErrFull = errors.New("desktop inbox is full")
var ErrUnsafe = errors.New("desktop inbox is not a private owned directory")
var ErrInvalid = errors.New("desktop event is invalid or too large")

type envelope struct {
	Kind  uint32          `json:"kind"`
	Event json.RawMessage `json:"event"`
}

// Publish atomically adds one kind-1 quota or kind-2 privacy observation. An
// empty inbox is disabled; a missing directory is not created. Existing claims
// and abandoned pending files count toward both bounds. A consumer must acquire
// the same .publish.lock before renaming a claim. Completed-event deletion may
// run without that lock; it can only make this count conservative.
// Event schema and the absence of secrets are the trusted producer's contract.
func Publish(inbox string, kind uint32, event any) error {
	if inbox == "" {
		return nil
	}
	if kind != 1 && kind != 2 {
		return ErrInvalid
	}
	if !filepath.IsAbs(inbox) || filepath.Clean(inbox) != inbox {
		return ErrUnsafe
	}
	before, err := os.Lstat(inbox)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !before.IsDir() || !privatePath(inbox, before) {
		return ErrUnsafe
	}
	canonical, err := filepath.EvalSymlinks(inbox)
	if err != nil || canonical != inbox {
		return ErrUnsafe
	}
	raw, err := json.Marshal(event)
	if err != nil || len(raw) < 2 || raw[0] != '{' || len(raw) > MaxEventBytes {
		return ErrInvalid
	}
	data, err := json.Marshal(envelope{kind, raw})
	if err != nil || len(data)+1 > MaxEventBytes {
		return ErrInvalid
	}
	data = append(data, '\n')

	lockPath := filepath.Join(inbox, ".publish.lock")
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() || !privatePath(lockPath, info) || info.Size() != 0 {
			return ErrUnsafe
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafe
	}
	lock, err := openLock(lockPath)
	if err != nil {
		return ErrUnsafe
	}
	defer lock.Close()
	if privatefile.Protect(lockPath, 0600) != nil {
		return ErrUnsafe
	}
	unlock, err := tryLock(lock)
	if err != nil {
		return err
	}
	defer unlock()
	if !samePrivate(inbox, before) || !sameLock(lockPath, lock) {
		return ErrUnsafe
	}
	if err := capacity(inbox, len(data)); err != nil {
		return err
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("desktop event name could not be created")
	}
	name := hex.EncodeToString(random[:])
	temp := filepath.Join(inbox, ".pending-"+name)
	final := filepath.Join(inbox, name+".json")
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("desktop event could not be staged")
	}
	defer os.Remove(temp)
	if err = privatefile.Protect(temp, 0600); err == nil {
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
	if err != nil || closeErr != nil {
		return errors.New("desktop event could not be saved")
	}
	if !samePrivate(inbox, before) || !sameLock(lockPath, lock) {
		return ErrUnsafe
	}
	if _, err := os.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		return errors.New("desktop event name already exists")
	}
	if err := os.Rename(temp, final); err != nil {
		return errors.New("desktop event could not be published")
	}
	// A directory-sync error means publication is uncertain. Callers do not
	// retry a new observation ID: the reader's durable deduplication owns that.
	if privatefile.SyncDir(inbox) != nil {
		return errors.New("desktop event publication could not be synced")
	}
	return nil
}

func samePrivate(path string, before os.FileInfo) bool {
	now, err := os.Lstat(path)
	return err == nil && now.IsDir() && os.SameFile(before, now) && privatePath(path, now)
}

func sameLock(path string, f *os.File) bool {
	disk, err := os.Lstat(path)
	held, heldErr := f.Stat()
	return err == nil && heldErr == nil && disk.Mode().IsRegular() && disk.Size() == 0 && os.SameFile(disk, held) && privatePath(path, disk)
}

func capacity(inbox string, added int) error {
	dir, err := os.Open(inbox)
	if err != nil {
		return ErrUnsafe
	}
	defer dir.Close()
	// One lock plus at most MaxEvents existing files. Never enumerate an
	// unbounded directory, even if another process puts unrelated files here.
	entries, err := dir.ReadDir(MaxEvents + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrUnsafe
	}
	count, size := 0, int64(0)
	for _, entry := range entries {
		if entry.Name() == ".publish.lock" {
			continue
		}
		info, err := os.Lstat(filepath.Join(inbox, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || !privatePath(filepath.Join(inbox, entry.Name()), info) {
			return ErrUnsafe
		}
		count++
		size += info.Size()
		if count >= MaxEvents || size+int64(added) > MaxInboxBytes {
			return ErrFull
		}
	}
	if len(entries) == MaxEvents+2 {
		return ErrFull
	}
	return nil
}
