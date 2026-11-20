package claudemaintenance

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

// A saved native config is also recognizable to the reference's account
// handover protocol: it waits for every <config>.run-*.lock holder. No token
// is read or copied, and a failed lock never silently permits the run.
func markRun(a account) (func(), error) {
	if err := validateAccount(a); err != nil {
		return nil, err
	}
	parent := filepath.Dir(a.ConfigDir)
	if !markerParent(parent) {
		return nil, ErrConfiguration
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	path := a.ConfigDir + ".run-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(nonce[:]) + ".lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrConfiguration
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(path) }
	if err = privatefile.Protect(path, 0600); err != nil {
		cleanup()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		cleanup()
		return nil, err
	}
	unlock, err := lockMarker(f)
	if err != nil {
		cleanup()
		return nil, err
	}
	if err = validateAccount(a); err != nil {
		unlock()
		cleanup()
		return nil, err
	}
	return func() {
		unlock()
		_ = f.Close()
		if current, e := os.Lstat(path); e == nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
	}, nil
}
