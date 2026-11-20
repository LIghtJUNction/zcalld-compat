//go:build !windows

package privacy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"syscall"
)

func firstPrivateNode(_ string, info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0 &&
		(info.IsDir() || info.Mode().IsRegular())
}
func firstOpen(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func firstFileIdentity(_ string, info os.FileInfo) (string, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errFirst
	}
	value := sha256.Sum256([]byte(fmt.Sprintf("zcalld:first:unix:v1:%x:%x", st.Dev, st.Ino)))
	return hex.EncodeToString(value[:]), nil
}
