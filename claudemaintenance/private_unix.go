//go:build !windows

package claudemaintenance

import (
	"os"
	"syscall"
)

func owned(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Geteuid())
}

// Claude's default current-account directory may legitimately be 0755. Its
// owner must still control all writes, and the profile remains private.
func currentDirectory(path string) bool {
	i, err := os.Lstat(path)
	return err == nil && i.IsDir() && i.Mode()&os.ModeSymlink == 0 && owned(i) && i.Mode().Perm()&0022 == 0
}
