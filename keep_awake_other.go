//go:build !windows && !darwin && !linux

package main

// preventSleep is a no-op on platforms with no known sleep-inhibit
// mechanism handled here (Windows uses SetThreadExecutionState, macOS
// uses caffeinate, Linux uses systemd-inhibit — see the other
// keep_awake_*.go files).
func preventSleep() {}
