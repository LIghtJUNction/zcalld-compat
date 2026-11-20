//go:build windows

package desktop

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Check directories as well as files without broadening or changing their ACL.
func privatePath(path string, _ os.FileInfo) bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != user.User.Sid.String() {
		return false
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace == nil {
			return false
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return false
		}
		if name := sid.String(); name != user.User.Sid.String() && name != "S-1-5-18" {
			return false
		}
	}
	return true
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
		return nil, ErrUnsafe
	}
	return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, overlap) }, nil
}
