//go:build unix && !linux

package plugintest

import (
	"os/exec"
	"strconv"
	"strings"
)

// ProcessStopped reports whether process pid is stopped by a signal (state
// T in ps): kill(SIGSTOP) returns before the process stops, and a process
// still running can answer a request.
func ProcessStopped(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()

	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "T")
}
