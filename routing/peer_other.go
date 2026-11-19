//go:build !linux

package routing

// Folder rules still work with private server-owned caller-key workspace
// bindings. No unverified request header or message is used as a substitute
// when this platform cannot prove a peer process's working directory.
func peerWorkspace(Peer) string { return "" }
