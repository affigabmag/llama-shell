//go:build linux

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// preventSleep uses systemd-inhibit (present on most modern Linux
// desktops) to block idle/sleep/lid-switch suspension for as long as the
// wrapped shell command keeps running. That command just polls this
// process's own PID and exits the moment llama-shell does, so the
// inhibitor releases automatically — no explicit cleanup needed. Silently
// does nothing on distros without systemd-inhibit (no systemd, or a
// minimal install) rather than failing.
func preventSleep() {
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return
	}
	pid := strconv.Itoa(os.Getpid())
	script := "while kill -0 " + pid + " 2>/dev/null; do sleep 5; done"
	cmd := exec.Command("systemd-inhibit", "--what=idle:sleep:handle-lid-switch",
		"--who=llama-shell", "--why=web server / telegram bot running",
		"--mode=block", "sh", "-c", script)
	_ = cmd.Start()
}
