//go:build windows

package main

import "syscall"

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecState = kernel32.NewProc("SetThreadExecutionState")
)

const (
	esContinuous      = 0x80000000
	esSystemRequired  = 0x00000001
	esDisplayRequired = 0x00000002
)

// preventSleep tells Windows not to sleep (or turn the display off) while
// llama-shell is running — the web server and Telegram bot both need the
// machine actually awake to be reachable, so a laptop going to sleep mid-
// session silently breaks both. ES_CONTINUOUS keeps this in effect until
// explicitly cleared or the process exits, so one call at startup is
// enough — no periodic refresh needed.
func preventSleep() {
	procSetThreadExecState.Call(uintptr(esContinuous | esSystemRequired | esDisplayRequired))
}
