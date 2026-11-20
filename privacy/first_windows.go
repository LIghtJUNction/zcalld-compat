//go:build windows

package privacy

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Check the existing ACL without broadening permissions or following a
// reparse-point leaf. Both private directories and regular files are accepted.
func firstPrivateNode(path string, info os.FileInfo) bool {
	if !info.IsDir() && !info.Mode().IsRegular() {
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
	for n := uint32(0); n < uint32(acl.AceCount); n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, n, &ace) != nil || ace == nil {
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
		if s := sid.String(); s != user.User.Sid.String() && s != "S-1-5-18" {
			return false
		}
	}
	return true
}
func firstOpen(path string) (*os.File, error) { return os.Open(path) }
