// Package plugin resolves and validates the plugin entries of a configuration
// document and runs and supervises their processes.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/getlaraplate/laraedge-sdk/manifest"
)

// ManifestFile is the name of the manifest next to a plugin binary.
const ManifestFile = "plugin.json"

// SumHex returns the lowercase hex SHA-256 of data.
func SumHex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// HashFile returns the lowercase hex SHA-256 of the file at path, read as a
// stream.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

// HashDir returns the SHA-256 of the plugin binary named in dir/plugin.json
// (with .exe on Windows) and of the plugin.json bytes.
func HashDir(dir string) (binarySum, manifestSum string, err error) {
	m, raw, err := manifest.Load(filepath.Join(dir, ManifestFile))
	if err != nil {
		return "", "", err
	}

	if strings.ContainsAny(m.Name, `/\`) || strings.Contains(m.Name, "..") {
		return "", "", errors.New("plugin.json: name must not contain a path separator or ..")
	}

	binaryPath := filepath.Join(dir, m.Name)
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	binarySum, err = HashFile(binaryPath)
	if err != nil {
		return "", "", fmt.Errorf("read the plugin binary: %w", err)
	}

	return binarySum, SumHex(raw), nil
}
