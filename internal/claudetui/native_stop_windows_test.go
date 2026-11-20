package claudetui

import "golang.org/x/sys/windows"

var ignoreStopCallback = windows.NewCallback(func(uint32) uintptr { return 1 })

// ConPTY closure sends CTRL_CLOSE_EVENT. Keep the synthetic CLI alive so
// the helper must TerminateProcess and wait for its process handle.
func nativeIgnoreStop() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler").Call(ignoreStopCallback, 1)
}
