//go:build windows

package claudemaintenance

import "os"

// privatefile.Check/CheckDirectory validate the current user's owner and DACL.
func owned(os.FileInfo) bool { return true }
