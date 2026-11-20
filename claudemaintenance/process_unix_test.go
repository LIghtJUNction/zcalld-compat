//go:build !windows

package claudemaintenance

import "syscall"

func ownedFixtureAlive(pid int) bool { return syscall.Kill(pid, 0) != syscall.ESRCH }
