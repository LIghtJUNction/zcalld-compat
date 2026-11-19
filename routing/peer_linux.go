//go:build linux

package routing

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func peerWorkspace(peer Peer) string {
	if peer.Kind == "unix" {
		if peer.PID <= 0 || peer.UID != uint32(os.Getuid()) || peer.Start == "" {
			return ""
		}
		return processWorkspace(peer.PID, peer.Start, "")
	}
	if peer.Kind != "tcp" {
		return ""
	}
	local, err1 := netip.ParseAddrPort(peer.Local)
	remote, err2 := netip.ParseAddrPort(peer.Remote)
	if err1 != nil || err2 != nil || !local.Addr().Unmap().IsLoopback() || !remote.Addr().Unmap().IsLoopback() {
		return ""
	}
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	inode := peerInode(remote, local)
	if inode == "" {
		return ""
	}
	// A header cannot select this PID. The kernel connection tuple must lead
	// to a socket FD owned by the same local account. Ambiguous forked owners
	// with different working directories deliberately have no trusted folder.
	names := directoryNames("/proc", 16384)
	deadline := time.Now().Add(300 * time.Millisecond)
	workspace := ""
	for _, name := range names {
		if time.Now().After(deadline) {
			return ""
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		path := filepath.Join("/proc", name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		native, ok := info.Sys().(*syscall.Stat_t)
		if !ok || native.Uid != uint32(os.Getuid()) {
			continue
		}
		value := processWorkspace(pid, "", inode)
		if value != "" {
			if workspace != "" && workspace != value {
				return ""
			}
			workspace = value
		}
	}
	// The client may have disconnected while its process table was inspected.
	if peerInode(remote, local) != inode {
		return ""
	}
	return workspace
}
func directoryNames(path string, limit int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	names, err := file.Readdirnames(limit + 1)
	if err != nil && err != io.EOF || len(names) > limit {
		return nil
	}
	return names
}
func procStart(pid int) string {
	path := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return ""
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		return ""
	}
	return fields[19]
}
func processWorkspace(pid int, start, inode string) string {
	root := filepath.Join("/proc", strconv.Itoa(pid))
	first := procStart(pid)
	if first == "" || start != "" && first != start {
		return ""
	}
	var socketFD string
	if inode != "" {
		for _, name := range directoryNames(filepath.Join(root, "fd"), 8192) {
			fd := filepath.Join(root, "fd", name)
			if target, err := os.Readlink(fd); err == nil && target == "socket:["+inode+"]" {
				socketFD = fd
				break
			}
		}
		if socketFD == "" {
			return ""
		}
	}
	dir, err := os.Readlink(filepath.Join(root, "cwd"))
	if err != nil || !filepath.IsAbs(dir) || strings.HasSuffix(dir, " (deleted)") || len(dir) > 4096 {
		return ""
	}
	if procStart(pid) != first {
		return ""
	}
	if socketFD != "" {
		if target, err := os.Readlink(socketFD); err != nil || target != "socket:["+inode+"]" {
			return ""
		}
	}
	return dir
}
func peerInode(client, server netip.AddrPort) string {
	for _, name := range []string{"tcp", "tcp6"} {
		file, err := os.Open(filepath.Join("/proc/net", name))
		if err != nil {
			continue
		}
		scan := bufio.NewScanner(io.LimitReader(file, 8<<20))
		scan.Buffer(make([]byte, 4096), 16384)
		result := ""
		for scan.Scan() {
			fields := strings.Fields(scan.Text())
			if len(fields) < 10 || fields[3] != "01" {
				continue
			}
			uid, err := strconv.ParseUint(fields[7], 10, 32)
			if err != nil || uint32(uid) != uint32(os.Getuid()) {
				continue
			}
			a, ok1 := procAddress(fields[1])
			b, ok2 := procAddress(fields[2])
			if ok1 && ok2 && a == client && b == server {
				result = fields[9]
				break
			}
		}
		file.Close()
		if result != "" {
			return result
		}
	}
	return ""
}
func procAddress(value string) (netip.AddrPort, bool) {
	host, port, ok := strings.Cut(value, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	n, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	bytes, err := hex.DecodeString(host)
	if err != nil || (len(bytes) != 4 && len(bytes) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(bytes); i += 4 {
		v := binary.BigEndian.Uint32(bytes[i : i+4])
		binary.NativeEndian.PutUint32(bytes[i:i+4], v)
	}
	address, ok := netip.AddrFromSlice(bytes)
	return netip.AddrPortFrom(address.Unmap(), uint16(n)), ok
}
