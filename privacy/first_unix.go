//go:build !windows

package privacy

import (
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
