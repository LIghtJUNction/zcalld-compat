//go:build !windows

// Package privatefile applies the host platform's private-file permissions.
package privatefile

import "os"

func Protect(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

func Check(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
