//go:build windows

package privatefile

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Private files and directories grant access only to this account and SYSTEM.
// POSIX mode bits alone do not enforce access restrictions on Windows.
func Protect(path string, mode os.FileMode) error {
	if mode.Perm()&0077 != 0 {
		return nil
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	flags := ""
	if info, err := os.Stat(path); err != nil {
		return err
	} else if info.IsDir() {
		flags = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")(A;" + flags + ";FA;;;SY)")
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("private file ACL is unavailable")
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}

func Check(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
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
		// GetAce returns an ACE within the security descriptor allocation.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return false
		}
		name := sid.String()
		if name != user.User.Sid.String() && name != "S-1-5-18" {
			return false
		}
	}
	return true
}

// Windows does not permit FlushFileBuffers on an ordinary directory handle.
// The file itself has already been flushed before the atomic rename.
func SyncDir(string) error { return nil }
