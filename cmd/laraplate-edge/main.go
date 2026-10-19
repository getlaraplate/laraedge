// Command laraplate-edge is the Laraplate shop-floor edge agent.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/swolley/laraplate-edge/internal/version"
)

const (
	exitOK        = 0
	exitFailure   = 1
	exitUsage     = 2
	defaultListen = "127.0.0.1:9180"
)

const usageText = `usage: laraplate-edge <command> [arguments]

commands:
  run --bootstrap <path>     run the agent
  config validate <file>     validate a configuration document
  status [--listen addr]     query a running agent (default ` + defaultListen + `)
  version                    print the version
`

var errNotImplemented = errors.New("not implemented yet")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stderr)
	case "config":
		return runConfig(args[1:], stderr)
	case "status":
		return runStatus(args[1:], stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "laraplate-edge: unknown command %q\n\n%s", args[0], usageText)
		return exitUsage
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	fmt.Fprintln(stdout, version.String())

	return exitOK
}

func runRun(args []string, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	bootstrap := fs.String("bootstrap", "", "path to the bootstrap file")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *bootstrap == "" || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "laraplate-edge run: --bootstrap <path> is required\n")
		return exitUsage
	}

	return notImplemented(stderr, "run")
}

func runConfig(args []string, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "validate" {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	return notImplemented(stderr, "config validate")
}

func runStatus(args []string, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	fs.String("listen", defaultListen, "address of the status endpoint")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	return notImplemented(stderr, "status")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("laraplate-edge "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	return fs
}

// parse parses args and returns the exit code to use when ok is false.
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}

		return exitUsage, false
	}

	return exitOK, true
}

func notImplemented(stderr io.Writer, command string) int {
	fmt.Fprintf(stderr, "laraplate-edge %s: %v\n", command, errNotImplemented)

	return exitFailure
}
