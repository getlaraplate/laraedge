package main

import (
	"fmt"
	"io"

	"github.com/getlaraplate/laraedge/internal/plugin"
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

	binarySum, manifestSum, err := plugin.HashDir(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge plugin hash: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "sha256: %s\nmanifest_sha256: %s\n", binarySum, manifestSum)

	return exitOK
}
