//go:build windows

package maintenance

import (
	"errors"
	"github.com/LIghtJUNction/zcalld-compat/internal/privatefile"
	"golang.org/x/sys/windows"
	"os"
)

func owned(path string, _ os.FileInfo) bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	return err == nil && owner != nil && owner.String() == user.User.Sid.String()
}
func privateDirectory(path string, info os.FileInfo) bool {
	return owned(path, info) && privatefile.Protect(path, 0700) == nil
}
func openLock(path string) (*os.File, error) { return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600) }
func tryLock(f *os.File) (func(), error) {
	handle := windows.Handle(f.Fd())
	overlap := new(windows.Overlapped)
	err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, overlap) }, nil
}
