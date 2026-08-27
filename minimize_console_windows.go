//go:build windows

package main

import "syscall"

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

const swMinimize = 6

// minimizeConsoleWindow minimizes this process's own console window —
// for --minimized, so llama-shell can be launched (e.g. from a startup
// script) without popping a window in front of the user.
func minimizeConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, uintptr(swMinimize))
}
