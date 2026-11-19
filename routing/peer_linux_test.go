//go:build linux

package routing

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPeerChildProcess(t *testing.T) {
	if os.Getenv("ZCALLD_ROUTING_PEER_FIXTURE") != "1" {
		return
	}
	connection, err := net.Dial("tcp", os.Getenv("ZCALLD_ROUTING_PEER_ADDRESS"))
	if err != nil {
		os.Exit(2)
	}
	defer connection.Close()
	connection.Write([]byte("r"))
	var data [1]byte
	connection.Read(data[:])
	os.Exit(0)
}
func TestTCPPeerWorkspaceUsesKernelSocketOwner(t *testing.T) {
	directory := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestPeerChildProcess$")
	child.Dir = directory
	child.Env = append(os.Environ(), "ZCALLD_ROUTING_PEER_FIXTURE=1", "ZCALLD_ROUTING_PEER_ADDRESS="+listener.Addr().String())
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	var ready [1]byte
	if _, err = connection.Read(ready[:]); err != nil {
		t.Fatal(err)
	}
	peer := Peer{Kind: "tcp", Local: connection.LocalAddr().String(), Remote: connection.RemoteAddr().String(), PID: os.Getpid()}
	if got := peerWorkspace(peer); got != directory {
		t.Fatalf("trusted cwd %q, expected child cwd %q (not supplied PID)", got, directory)
	}
	peer.Remote = "192.0.2.1:1234"
	if got := peerWorkspace(peer); got != "" {
		t.Fatal("nonlocal peer acquired a trusted directory")
	}
	connection.Write([]byte("x"))
	if err = child.Wait(); err != nil {
		t.Fatal(err)
	}
}
func TestUnixIdentityRequiresCurrentStartTime(t *testing.T) {
	pid := os.Getpid()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{Kind: "unix", PID: pid, UID: uint32(os.Getuid()), Start: procStart(pid)}
	if got := peerWorkspace(peer); got != cwd {
		t.Fatalf("unix cwd %q, want %q", got, cwd)
	}
	peer.Start = "0"
	if peerWorkspace(peer) != "" {
		t.Fatal("reused PID identity was trusted")
	}
}
func TestProcAddressEndianAndMappedV4(t *testing.T) {
	for text, want := range map[string]string{"0100007F:0D61": "127.0.0.1:3425", "0000000000000000FFFF00000100007F:0D61": "127.0.0.1:3425"} {
		got, ok := procAddress(text)
		if !ok || got.String() != want {
			t.Fatal(fmt.Sprint(text, got, ok))
		}
	}
	if _, ok := procAddress("invalid"); ok {
		t.Fatal("invalid endpoint accepted")
	}
	if processWorkspace(os.Getpid(), procStart(os.Getpid()), "not-a-socket") != "" {
		t.Fatal("missing socket accepted")
	}
	if filepath.IsAbs("relative") {
		t.Fatal("fixture assumption")
	}
}
