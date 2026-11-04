package plugin_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processStopped reports whether every thread of process pid is stopped by
// a signal (state T in /proc/<pid>/task/<tid>/stat): kill(SIGSTOP) returns
// before the threads stop, and a thread still running can answer a request.
func processStopped(pid int) bool {
	tasks, err := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "stat"))
	if err != nil || len(tasks) == 0 {
		return false
	}
	for _, task := range tasks {
		data, err := os.ReadFile(task)
		if err != nil {
			return false
		}
		// The state follows the command name, which is in parentheses and
		// may itself contain them.
		rest := string(data[strings.LastIndexByte(string(data), ')')+1:])
		if fields := strings.Fields(rest); len(fields) == 0 || fields[0] != "T" {
			return false
		}
	}

	return true
}
