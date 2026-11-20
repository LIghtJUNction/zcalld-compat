// Retained pure upstream path mapping for VT/parser code. No discovery,
// no account lookup, and no WSL capability is advertised by the wrapper.
package wslrun

import "strings"

type Tool struct {
	Distro string // as wsl.exe -l names it
	Path   string // the command, inside the distro
	PATH   string // the distro's PATH it was found on, which it may need (node)
	Mount  string // where Windows' drives are mounted: "/mnt/"
	Host   string // the Windows host's address inside the distro, "" when it is 127.0.0.1 (HostLoopback)
	Root   bool   // the distro's default user, as whom the command runs, is root
	Home   string // that user's home folder, inside the distro
}

func (t Tool) Linux(win string) string {
	if len(win) < 2 || win[1] != ':' || !isLetter(win[0]) {
		return win
	}
	rest := strings.ReplaceAll(win[2:], `\`, "/")
	mount := t.Mount
	if mount == "" {
		mount = "/mnt/"
	}
	return mount + strings.ToLower(win[:1]) + "/" + strings.TrimPrefix(rest, "/")
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
