package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/clock"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/opcua"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/plugin"
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

	if doc, boot, cfg, ok := pluginInputOf(*configFile, *bootstrapPath, positional[0]); ok {
		return browsePlugin(doc, boot, cfg, *nodeFlag, *depth, stdout, stderr)
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

// errPluginNoBrowse is what browse says of a plugin whose manifest does not
// declare browse.
const errPluginNoBrowse = "the plugin of this input does not support browse"

// pluginInputOf returns the document and the input id when it is a plugin
// input of a document that loads cleanly; otherwise ok is false and the
// OPC UA path reports whatever is wrong, in its own words.
func pluginInputOf(configFile, bootstrapPath, id string) (doc config.Document, boot bootstrap.File, cfg config.Input, ok bool) {
	if configFile == "" && bootstrapPath == "" {
		return doc, boot, cfg, false
	}

	doc, boot, msgs, err := probeDocument(configFile, bootstrapPath)
	if err != nil || len(msgs) > 0 {
		return doc, boot, cfg, false
	}

	i := slices.IndexFunc(doc.Inputs, func(in config.Input) bool { return in.ID == id })
	if i < 0 || !strings.HasPrefix(doc.Inputs[i].Driver, plugin.DriverPrefix) {
		return doc, boot, cfg, false
	}

	return doc, boot, doc.Inputs[i], true
}

// browsePlugin lists the nodes a plugin input offers below node: one line per
// node, "<address>  <name>  <type>  <access>", the address being usable as the
// address of a point. depth counts levels below the start node, as for
// OPC UA; the plugin protocol counts from zero, the children only.
func browsePlugin(doc config.Document, boot bootstrap.File, cfg config.Input, node string, depth int, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge browse: "

	log := logging.New(stderr, slog.LevelWarn)

	ctx, release := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer release()

	in, closePlugin, err := openPluginInput(ctx, doc, boot, cfg, true, log)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}
	defer closePlugin()

	browser, ok := in.(plugin.Browser)
	if !ok {
		fmt.Fprintf(stderr, "%s%s\n", prefix, errPluginNoBrowse)
		return exitFailure
	}

	ctx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()

	nodes, err := browser.Browse(ctx, node, depth-1)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	for _, n := range nodes {
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", n.Address, n.Name, cmp.Or(n.Type, "?"), cmp.Or(n.Access, "?"))
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
