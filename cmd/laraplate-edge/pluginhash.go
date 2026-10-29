package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/getlaraplate/laraedge-sdk/manifest"
)

// runPlugin runs "plugin hash <dir>".
func runPlugin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "hash" {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	return runPluginHash(args[1:], stdout, stderr)
}

// runPluginHash prints the two hashes a configuration document pins for a
// plugin: the binary named in <dir>/plugin.json (with .exe on Windows) and the
// manifest file itself.
func runPluginHash(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plugin hash", stderr)
	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	binarySum, manifestSum, err := hashPlugin(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge plugin hash: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "sha256: %s\nmanifest_sha256: %s\n", binarySum, manifestSum)

	return exitOK
}

// hashPlugin returns the lowercase hex SHA-256 of the plugin binary and of
// the plugin.json bytes in dir.
func hashPlugin(dir string) (binarySum, manifestSum string, err error) {
	manifestPath := filepath.Join(dir, "plugin.json")
	m, raw, err := manifest.Load(manifestPath)
	if err != nil {
		return "", "", err
	}

	binaryPath := filepath.Join(dir, m.Name)
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return "", "", fmt.Errorf("read the plugin binary: %w", err)
	}

	return sumHex(data), sumHex(raw), nil
}

func sumHex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
