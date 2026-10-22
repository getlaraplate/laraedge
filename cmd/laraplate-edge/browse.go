package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/input/opcua"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

// browseTimeout bounds the connection and the whole walk.
const browseTimeout = 2 * time.Minute

// runBrowse runs "browse <input>": it walks the hierarchical references of an
// OPC UA server from the Objects folder, or from --node, and prints one line
// per node, indented two spaces per level, as
// "<NodeId>  <BrowseName>  <NodeClass>[  <DataType>  <access>]", the last
// two for a variable (access r, w, rw or -; "?" for an unreadable
// attribute). It connects like probe (the trust check included) and reads no
// value.
func runBrowse(args []string, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge browse: "

	fs := newFlagSet("browse", stderr)
	bootstrapPath := fs.String("bootstrap", "", "path to the bootstrap file; its data_dir holds the PKI and the last valid document")
	configFile := fs.String("config", "", "configuration document to use instead of the last valid one")
	nodeFlag := fs.String("node", "", "NodeId to start from (default: the Objects folder, i=85)")
	depth := fs.Int("depth", opcua.DefaultBrowseDepth, fmt.Sprintf("levels to print below the start node, 1 to %d", opcua.MaxBrowseDepth))

	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	if *depth < 1 || *depth > opcua.MaxBrowseDepth {
		fmt.Fprintf(stderr, "%s--depth: want a number from 1 to %d\n", prefix, opcua.MaxBrowseDepth)
		return exitUsage
	}

	var start opcua.Node
	if *nodeFlag != "" {
		n, err := opcua.ParseNode(*nodeFlag)
		if err != nil {
			fmt.Fprintf(stderr, "%s--node: %v\n", prefix, err)
			return exitUsage
		}

		start = n
	}

	target, code, ok := openOPCUAInput("browse", stderr, *configFile, *bootstrapPath, positional[0])
	if !ok {
		return code
	}

	log := logging.New(stderr, slog.LevelWarn)
	defer routeLibraryLogs(log)()

	in, err := opcua.New(target.input, clock.Real(), opcua.Options{
		DataDir:  target.boot.DataDir,
		AgentID:  target.boot.AgentID,
		Resolver: secrets.NewResolver(),
		Logger:   log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}
	defer func() { _ = in.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), browseTimeout)
	defer cancel()

	truncated, err := in.Browse(ctx, start, *depth, func(e opcua.BrowseEntry) {
		fmt.Fprintln(stdout, browseLine(e))
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	if truncated {
		fmt.Fprintf(stdout, "... stopped after %d nodes\n", opcua.BrowseMaxNodes)
	}

	return exitOK
}

// browseLine is the printed line of e: the data type and access columns
// appear for a variable whose attributes were read, "?" standing for the
// one that was unreadable.
func browseLine(e opcua.BrowseEntry) string {
	line := strings.Repeat("  ", e.Depth) + e.NodeID + "  " + e.Name + "  " + e.Class
	if e.DataType != "" || e.Access != "" {
		line += "  " + cmp.Or(e.DataType, "?") + "  " + cmp.Or(e.Access, "?")
	}

	return line
}
