// Command laraplate-edge is the Laraplate shop-floor edge agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/secrets"
	"github.com/getlaraplate/laraedge/internal/transform"
	"github.com/getlaraplate/laraedge/internal/version"
)

const (
	exitOK        = 0
	exitFailure   = 1
	exitUsage     = 2
	defaultListen = "127.0.0.1:9180"
)

const usageText = `usage: laraplate-edge <command> [arguments]

commands:
  run --bootstrap <path> [--log-level debug|info|warn|error]
                             run the agent (default log level info)
  config validate <file>     validate a configuration document
  config test <file> --input <samples.jsonl>
                             replay recorded samples (JSON Lines) through the
                             transform chains of a document; the resulting
                             samples go to stdout, in the same format
  probe <input> (--config <file> | --bootstrap <path>) [--record <file>]
                             read every point of an input once and print the
                             samples; the document is the --config file (it
                             wins over --bootstrap, whose data_dir and
                             agent_id an opcua input still uses), else the
                             last valid one in the bootstrap's data_dir;
                             --record writes the raw samples as JSON Lines
  browse <input> (--config <file> | --bootstrap <path>) [--node <nodeid>] [--depth <n>]
                             list the nodes of an OPC UA server, indented by
                             depth, from the Objects folder or --node
                             (nsu=<uri>;<id> works); --depth 1 to 10, default 2;
                             stops after 1000 nodes; reads no value
  opcua trust <input> (--config <file> | --bootstrap <path>) [--thumbprint <hex>]
                             show the certificate an OPC UA server presents and
                             trust it after a yes on stdin, or without asking
                             when --thumbprint equals the server's
  opcua trust --list (--config <file> | --bootstrap <path>)
                             list the trusted certificates
  status [--listen addr]     query a running agent (default ` + defaultListen + `);
                             the token, if any, from ` + statusTokenEnv + `
  version                    print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, os.Stdin, stdout, stderr)
}

// runWithInput is run with the standard input the interactive commands read.
func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "browse":
		return runBrowse(args[1:], stdout, stderr)
	case "opcua":
		return runOPCUA(args[1:], stdin, stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
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

// runRun runs the agent. Its logs go to stdout as JSON lines (spec 10.5); the
// usage and the error that stops it go to stderr.
func runRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", stderr)
	bootstrap := fs.String("bootstrap", "", "path to the bootstrap file")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if *bootstrap == "" || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "laraplate-edge run: --bootstrap <path> is required\n")
		return exitUsage
	}
	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge run: --log-level: %v\n", err)
		return exitUsage
	}
	deps := productionDeps()
	deps.logger = logging.New(stdout, level)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reload := make(chan struct{}, 1)
	stopReload := notifyReload(reload)
	defer stopReload()

	if err := runAgent(ctx, *bootstrap, deps, reload, stdout); err != nil {
		fmt.Fprintf(stderr, "laraplate-edge run: %v\n", err)
		return exitFailure
	}

	return exitOK
}

// runConfig runs "config validate <file>" and "config test <file>".
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "test" {
		return runConfigTest(args[1:], stdout, stderr)
	}

	return runConfigValidate(args, stdout, stderr)
}

// runConfigValidate runs "config validate <file>": every validation layer,
// the input drivers' own included (decision M1), every message on stderr
// prefixed by the file name, exit 0 when there is none.
func runConfigValidate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "validate" {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	file := args[1]
	doc, msgs, err := parseDocument(file)
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge config validate: %v\n", err)
		return exitFailure
	}
	if len(msgs) == 0 {
		msgs = append(checkDocument(doc), checkInputs(doc, file)...)
	}
	if len(msgs) > 0 {
		printMessages(stderr, file, msgs)
		return exitFailure
	}

	fmt.Fprintf(stdout, "%s: valid\n", file)

	return exitOK
}

// loadDocument reads file and applies every validation layer: config.Parse,
// then config.CheckBuild and transform.CheckDocument (decision P1), which
// report together. The messages are those of the document; the error is a
// file that cannot be read or is not a document at all.
func loadDocument(file string) (config.Document, []string, error) {
	doc, msgs, err := parseDocument(file)
	if err != nil {
		return config.Document{}, nil, err
	}
	if len(msgs) == 0 {
		msgs = checkDocument(doc)
	}

	return doc, msgs, nil
}

// parseDocument reads file and parses it (config.Parse): the messages are
// those of the schema and semantic layers.
func parseDocument(file string) (config.Document, []string, error) {
	data, err := readDocument(file)
	if err != nil {
		return config.Document{}, nil, err
	}

	return config.Parse(data)
}

// checkDocument applies the layers after config.Parse to a parsed document:
// config.CheckBuild and transform.CheckDocument.
func checkDocument(doc config.Document) []string {
	return append(config.CheckBuild(doc), transform.CheckDocument(doc)...)
}

// checkInputs builds every input of doc through the driver registry of run
// and reports the factories' errors as the supervisor does, "inputs.<i>:
// <error>" (decision M1). Factories only validate and never connect (core
// R47), so this works offline. Relative paths of a connection resolve
// against the directory of file, as with the file provider. An input whose
// driver this build lacks is left to config.CheckBuild. No factory resolves a
// secret nor logs, so the resolver is never used and the logger discards.
func checkInputs(doc config.Document, file string) []string {
	boot := bootstrap.File{Config: bootstrap.Config{Provider: bootstrap.ProviderFile, File: file}}
	drivers := inputDrivers(boot, secrets.NewResolver(), slog.New(slog.DiscardHandler), nil)

	var msgs []string
	for i, cfg := range doc.Inputs {
		factory := drivers[cfg.Driver]
		if factory == nil {
			continue
		}

		in, err := factory(cfg, clock.Real())
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("inputs.%d: %v", i, err))
			continue
		}
		_ = in.Close()
	}

	return msgs
}

// printMessages writes each message on its own line, prefixed by the file.
func printMessages(w io.Writer, file string, msgs []string) {
	for _, m := range msgs {
		fmt.Fprintf(w, "%s: %s\n", file, m)
	}
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

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	listen := fs.String("listen", defaultListen, "address of the status endpoint")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	out, err := fetchStatus(*listen, os.Getenv(statusTokenEnv))
	if err != nil {
		fmt.Fprintf(stderr, "laraplate-edge status: %v\n", err)
		return exitFailure
	}
	_, _ = stdout.Write(out)

	return exitOK
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("laraplate-edge "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)

	return fs
}

// parseArgs parses args like parse, but accepts flags before, between and
// after the positional arguments, which it returns.
func parseArgs(fs *flag.FlagSet, args []string) (positional []string, code int, ok bool) {
	for {
		if code, ok := parse(fs, args); !ok {
			return nil, code, false
		}
		if fs.NArg() == 0 {
			return positional, exitOK, true
		}

		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
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
