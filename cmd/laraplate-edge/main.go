// Command laraplate-edge is the Laraplate shop-floor edge agent.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/swolley/laraplate-edge/internal/config"
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
		return runConfig(args[1:], stdout, stderr)
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

// runConfig runs "config validate <file>": both validation layers, every
// message on stderr prefixed by the file name, exit 0 when there is none.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "validate" {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	file := args[1]
	data, err := readDocument(file)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config validate: %v\n", err)
		return exitFailure
	}

	doc, msgs, err := config.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config validate: %v\n", err)
		return exitFailure
	}
	if len(msgs) == 0 {
		msgs = config.CheckBuild(doc)
	}
	if len(msgs) > 0 {
		for _, m := range msgs {
			fmt.Fprintf(stderr, "%s: %s\n", file, m)
		}
		return exitFailure
	}

	fmt.Fprintf(stdout, "%s: valid\n", file)

	return exitOK
}

// readDocument reads at most one byte more than config.MaxDocumentSize, so
// Parse reports an oversized file without reading all of it.
func readDocument(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, config.MaxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}

	return data, nil
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
