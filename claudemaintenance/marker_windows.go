//go:build windows

package claudemaintenance

import (
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"golang.org/x/sys/windows"
	"os"
)

func lockMarker(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	o := new(windows.Overlapped)
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, o); err != nil {
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, o) }, nil
}
func markerParent(path string) bool { return noLinks(path) && privatefile.CheckDirectory(path) }
