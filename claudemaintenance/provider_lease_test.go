package claudemaintenance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderLeaseCloseWaitsForTheQuotaOperationAndObservesItsDrainResult(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown"}[unknown], func(t *testing.T) {
			f := nativeFixture(t)
			key := bindNative(t, f)
			c, a, stamp, err := (Host{Root: f.root}).selected(key)
			if err != nil {
				t.Fatal(err)
			}
			release, err := markRun(a)
			if err != nil {
				t.Fatal(err)
			}
			// This synthetic lease stands at the exact serialized process-finalizer
			// boundary. No arbitrary native process is left running by the test.
			l := &ProviderLease{host: Host{Root: f.root}, key: key, stamp: stamp, config: c, account: a, release: release}
			defer release()
			pattern := filepath.Join(filepath.Dir(a.ConfigDir), filepath.Base(a.ConfigDir)+".run-*.lock")
			markers, err := filepath.Glob(pattern)
			if err != nil || len(markers) != 1 {
				t.Fatal(markers, err)
			}
			l.mu.Lock()
			started := make(chan struct{})
			closed := make(chan error, 1)
			go func() { close(started); closed <- l.Close() }()
			<-started
			select {
			case <-closed:
				t.Fatal("released marker before quota finalizer completed")
			default:
			}
			if _, err = os.Stat(markers[0]); err != nil {
				t.Fatal("quota operation lost its marker", err)
			}
			l.uncertain = unknown
			l.mu.Unlock()
			err = <-closed
			if unknown {
				if !errors.Is(err, ErrProviderDrain) {
					t.Fatal("lost uncertain finalizer classification", err)
				}
				if _, err = os.Stat(markers[0]); err != nil {
					t.Fatal("unknown process marker was removed", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(markers[0]); !os.IsNotExist(err) {
					t.Fatal("confirmed process marker was retained", err)
				}
			}
		})
	}
}
