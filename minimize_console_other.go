//go:build !windows

package main

// minimizeConsoleWindow is a no-op on non-Windows platforms — there's no
// single portable "minimize the terminal window" API, and this is a
// nice-to-have for the --minimized flag, not a core feature.
func minimizeConsoleWindow() {}
