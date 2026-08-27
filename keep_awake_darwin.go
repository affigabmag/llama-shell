//go:build darwin

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// preventSleep spawns macOS's built-in caffeinate, tied via -w to this
// process's own PID so it exits automatically the moment llama-shell does
// — no explicit cleanup/shutdown hook needed. -d/-i/-m/-s cover display,
// idle, disk, and system sleep respectively.
func preventSleep() {
	pid := strconv.Itoa(os.Getpid())
	cmd := exec.Command("caffeinate", "-d", "-i", "-m", "-s", "-w", pid)
	_ = cmd.Start()
}
