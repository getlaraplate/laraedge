// Package plugin resolves, validates and (in later tasks) runs the plugin
// processes of a configuration document.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", "", fmt.Errorf("read the plugin binary: %w", err)
	}

	return SumHex(data), SumHex(raw), nil
}
