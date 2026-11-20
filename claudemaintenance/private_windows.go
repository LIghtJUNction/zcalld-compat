//go:build windows

package claudemaintenance

import "os"

import "github.com/LIghtJUNction/zcalld-compat/internal/privatefile"

// privatefile.Check/CheckDirectory validate the current user's owner and DACL.
func owned(os.FileInfo) bool { return true }

func currentDirectory(path string) bool { return privatefile.CheckDirectory(path) }
