package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/getlaraplate/laraplate-edge/internal/atomicfile"
)

// dataDirFormat is the data_dir layout this agent writes and reads (spec
// 10.1). A later format migrates forward at start; an older agent refuses a
// newer data_dir instead of corrupting it.
const dataDirFormat = 1

// formatFile is the file of data_dir holding its format version.
const formatFile = "format"

// prepareDataDir creates dir when missing, writes its format version when
// absent and refuses a data_dir of a newer format.
func prepareDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("data_dir %s: %w", dir, err)
	}

	data, err := os.ReadFile(filepath.Join(dir, formatFile))
	if errors.Is(err, fs.ErrNotExist) {
		if err := atomicfile.Write(dir, formatFile, []byte(strconv.Itoa(dataDirFormat)+"\n")); err != nil {
			return fmt.Errorf("data_dir %s: write the format version: %w", dir, err)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("data_dir %s: read the format version: %w", dir, err)
	}

	format, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || format < 1 {
		return fmt.Errorf("data_dir %s: %s does not hold a format version", dir, formatFile)
	}
	if format > dataDirFormat {
		return fmt.Errorf("data_dir %s has format %d, newer than this agent supports (%d): run a newer agent", dir, format, dataDirFormat)
	}

	return nil
}
