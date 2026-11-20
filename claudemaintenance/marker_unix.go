//go:build !windows

package claudemaintenance

import (
	"os"
	"syscall"
)

func lockMarker(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
func markerParent(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && noLinks(path) && info.IsDir() && owned(info) && info.Mode().Perm()&0022 == 0
}
