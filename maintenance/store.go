package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
)

type Store struct {
	Directory string
	// Used only by in-package crash/failure fixtures, after durable writes.
	afterSave func(Document) error
}

type transaction struct {
	store   Store
	doc     Document
	dirInfo os.FileInfo
	lock    *os.File
	unlock  func()
}

func validateDocument(d Document) error {
	if d.Version != SchemaVersion || d.Policy.Accounts == nil || d.Ledger == nil || len(d.Policy.Accounts) > MaxAccounts || len(d.Ledger) > MaxAccounts || len(d.LegacyWarmup)+len(d.LegacyAutoReset)+len(d.LegacyResetsUsed) > 2<<20 {
		return ErrInvalid
	}
	if d.Policy.Enabled && d.Policy.EnabledAt.IsZero() {
		return ErrInvalid
	}
	for account, p := range d.Policy.Accounts {
		if !accountPattern.MatchString(account) || p.ResetLeadMinutes < 3 || p.ResetLeadMinutes > 1440 {
			return ErrInvalid
		}
		if p.FiveHourWarmup && !p.WeeklyWarmup {
			return ErrInvalid
		}
		clocks, err := normalizedClocks(p.WarmAts)
		if err != nil || len(clocks) != len(p.WarmAts) {
			return ErrInvalid
		}
		for i := range clocks {
			if clocks[i] != p.WarmAts[i] {
				return ErrInvalid
			}
		}
	}
	for account, l := range d.Ledger {
		if !accountPattern.MatchString(account) || l == nil || len(l.Windows) > 64 || len(l.CompletedCards) > 1024 {
			return ErrInvalid
		}
		for n, w := range l.Windows {
			if n == "" || len(n) > 120 || math.IsNaN(w.Used) || math.IsInf(w.Used, 0) || w.Used < 0 || w.Used > 100 || w.Failed < 0 || w.Idle < 0 {
				return ErrInvalid
			}
		}
		if p := l.Warm; p != nil {
			if (p.Phase != "prepared" && p.Phase != "sending" && p.Phase != "uncertain") || len(p.Windows) == 0 || len(p.Windows) > 64 || p.PreparedAt.IsZero() {
				return ErrInvalid
			}
			if len(p.Daily) > len(p.Windows) {
				return ErrInvalid
			}
			for name, day := range p.Daily {
				found := false
				for _, window := range p.Windows {
					found = found || name == window
				}
				if !found {
					return ErrInvalid
				}
				if _, err := time.Parse("2006-01-02 15:04", day); err != nil {
					return ErrInvalid
				}
			}
		}
		if p := l.Reset; p != nil {
			if (p.Phase != "prepared" && p.Phase != "sending" && p.Phase != "uncertain") || p.CreditID == "" || len(p.CreditID) > 512 || !redeemPattern.MatchString(p.RedeemRequestID) {
				return ErrInvalid
			}
			if p.Purpose == "week" {
				if p.WeekUntil.IsZero() {
					return ErrInvalid
				}
			} else if (p.Purpose != "" && p.Purpose != "expiring") || p.ExpiresAt.IsZero() {
				return ErrInvalid
			}
		}
	}
	return nil
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !privatefile.Check(path) || !owned(path, info) {
		return nil, ErrUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, ErrUnsafe
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, ErrInvalid
	}
	return b, nil
}

// Read performs no directory/lock creation and never starts the host.
func (s Store) Read() (Document, error) {
	b, err := readPrivate(filepath.Join(s.Directory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return emptyDocument(), nil
	}
	if err != nil {
		return Document{}, err
	}
	d := Document{}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil {
		return Document{}, ErrInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Document{}, ErrInvalid
	}
	if err = validateDocument(d); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (s Store) acquire(ctx context.Context, create bool) (*transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(s.Directory, 0700); err != nil {
			return nil, err
		}
	}
	di, err := os.Lstat(s.Directory)
	if err != nil {
		return nil, err
	}
	if !di.IsDir() || di.Mode()&os.ModeSymlink != 0 || !privateDirectory(s.Directory, di) {
		return nil, ErrUnsafe
	}
	path := filepath.Join(s.Directory, ".run.lock")
	before, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && (!before.Mode().IsRegular() || !privatefile.Check(path) || !owned(path, before)) {
		return nil, ErrUnsafe
	}
	f, err := openLock(path)
	if err != nil {
		return nil, err
	}
	closeFailure := func(e error) (*transaction, error) { f.Close(); return nil, e }
	fi, err := f.Stat()
	if err != nil {
		return closeFailure(err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, fi) || !fi.Mode().IsRegular() || !owned(path, fi) {
		return closeFailure(ErrUnsafe)
	}
	if err = privatefile.Protect(path, 0600); err != nil {
		return closeFailure(err)
	}
	unlock, err := tryLock(f)
	if err != nil {
		return closeFailure(err)
	}
	tx := &transaction{store: s, dirInfo: di, lock: f, unlock: unlock}
	if err = tx.samePaths(); err != nil {
		tx.close()
		return nil, err
	}
	tx.doc, err = s.Read()
	if err != nil {
		tx.close()
		return nil, err
	}
	return tx, nil
}
func (t *transaction) close() { t.unlock(); t.lock.Close() }
func (t *transaction) samePaths() error {
	di, err := os.Lstat(t.store.Directory)
	if err != nil || !os.SameFile(t.dirInfo, di) {
		return ErrUnsafe
	}
	held, err := t.lock.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(filepath.Join(t.store.Directory, ".run.lock"))
	if err != nil || !os.SameFile(held, named) {
		return ErrUnsafe
	}
	return nil
}
func (t *transaction) save() error {
	if err := validateDocument(t.doc); err != nil {
		return err
	}
	if err := t.samePaths(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t.doc, "", "  ")
	if err != nil || len(b)+1 > MaxFileBytes {
		return ErrInvalid
	}
	path := filepath.Join(t.store.Directory, "state.json")
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() || !privatefile.Check(path) || !owned(path, info) {
			return ErrUnsafe
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, err := os.CreateTemp(t.store.Directory, ".state-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = privatefile.Protect(temp, 0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = t.samePaths(); err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	if err = privatefile.SyncDir(t.store.Directory); err != nil {
		return err
	}
	if t.store.afterSave != nil {
		return t.store.afterSave(t.doc)
	}
	return nil
}

func (s Store) SetEnabled(ctx context.Context, enabled bool, now time.Time) error {
	tx, err := s.acquire(ctx, false)
	if err != nil {
		return err
	}
	defer tx.close()
	if enabled && len(tx.doc.Policy.Accounts) == 0 {
		return ErrInvalid
	}
	if tx.doc.Policy.Enabled == enabled {
		return nil
	}
	tx.doc.Policy.Enabled = enabled
	if enabled {
		tx.doc.Policy.EnabledAt = now.UTC()
	}
	return tx.save()
}
