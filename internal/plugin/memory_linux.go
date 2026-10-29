package plugin

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
)

// processMemory returns the resident set size of process pid in bytes, read
// from VmRSS in /proc/<pid>/status.
func processMemory(pid int) (int64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		rest, ok := bytes.CutPrefix(scanner.Bytes(), []byte("VmRSS:"))
		if !ok {
			continue
		}
		fields := bytes.Fields(rest)
		if len(fields) != 2 || string(fields[1]) != "kB" {
			return 0, errors.New("/proc status: unexpected VmRSS line")
		}
		kib, err := strconv.ParseInt(string(fields[0]), 10, 64)
		if err != nil {
			return 0, errors.New("/proc status: unexpected VmRSS value")
		}

		return kib * 1024, nil
	}

	return 0, errors.New("/proc status: no VmRSS line")
}
