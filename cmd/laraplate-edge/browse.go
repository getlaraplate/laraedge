package main

import (
	"cmp"
	"context"
	"flag"
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
	"github.com/getlaraplate/laraedge/internal/input/mqtt"
	"github.com/getlaraplate/laraedge/internal/input/opcua"
	"github.com/getlaraplate/laraedge/internal/logging"
	"github.com/getlaraplate/laraedge/internal/plugin"
	"github.com/getlaraplate/laraedge/internal/secrets"
)

// browseTimeout bounds the connection and the whole walk.
const browseTimeout = 2 * time.Minute

// mqttBrowseClock times the --duration of an mqtt browse, which starts once
// the broker has answered the subscription. A variable so the tests can
// collect for less than the 1 second --duration allows with a faster clock;
// the real clock for users. Tests that change it must not run in parallel.
var mqttBrowseClock = clock.Real()

// runBrowse runs "browse <input>": it walks the hierarchical references of an
// OPC UA server from the Objects folder, or from --node, and prints one line
// per node, indented two spaces per level, as
// "<NodeId>  <BrowseName>  <NodeClass>[  <DataType>  <access>]", the last
// two for a variable (access r, w, rw or -; "?" for an unreadable
// attribute), and "  (not browsed: <status>)" after a node whose children
// the server refused to list; a start node without children prints
// browseNothingBelow. It connects like probe (the trust check included) and
// reads no value.
func runBrowse(args []string, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge browse: "

	fs := newFlagSet("browse", stderr)
	bootstrapPath := fs.String("bootstrap", "", "path to the bootstrap file; its data_dir holds the PKI and the last valid document")
	configFile := fs.String("config", "", "configuration document to use instead of the last valid one")
	nodeFlag := fs.String("node", "", "NodeId to start from (default: the Objects folder, i=85)")
	depth := fs.Int("depth", opcua.DefaultBrowseDepth, fmt.Sprintf("levels to print below the start node, 1 to %d", opcua.MaxBrowseDepth))
	filter := fs.String("filter", mqtt.DefaultBrowseFilter, "mqtt topic filter to subscribe to (default "+mqtt.DefaultSparkplugBrowseFilter+" for an input with format sparkplug_b)")
	duration := fs.Duration("duration", mqtt.DefaultBrowseDuration, "mqtt: how long to collect messages, 1s to 5m")

	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	doc, boot, cfg, found := loadedInput(*configFile, *bootstrapPath, positional[0])
	if found && cfg.Driver == config.DriverMQTT {
		if given["node"] || given["depth"] {
			fmt.Fprintf(stderr, "%s%s\n", prefix, errMQTTBrowseFlags)
			return exitUsage
		}

		conn, err := mqtt.ParseConnection(cfg.Connection, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "%smqtt input %q: %v\n", prefix, cfg.ID, err)
			return exitFailure
		}
		sparkplug := conn.Format == mqtt.FormatSparkplugB

		// An input with format sparkplug_b browses spBv1.0/# unless --filter
		// says otherwise (decision S14).
		if sparkplug && !given["filter"] {
			*filter = mqtt.DefaultSparkplugBrowseFilter
		}

		return browseMQTT(doc, boot, cfg, *configFile, *bootstrapPath, *filter, *duration, sparkplug, stdout, stderr)
	}
	if found && (given["filter"] || given["duration"]) {
		fmt.Fprintf(stderr, "%s%s\n", prefix, errOtherBrowseFlags)
		return exitUsage
	}

	if *depth < 1 || *depth > opcua.MaxBrowseDepth {
		fmt.Fprintf(stderr, "%s--depth: want a number from 1 to %d\n", prefix, opcua.MaxBrowseDepth)
		return exitUsage
	}

	if found && strings.HasPrefix(cfg.Driver, plugin.DriverPrefix) {
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

	printed := 0

	truncated, err := in.Browse(ctx, start, *depth, func(e opcua.BrowseEntry) {
		printed++
		fmt.Fprintln(stdout, browseLine(e))
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	if printed == 0 {
		fmt.Fprintln(stdout, browseNothingBelow)
	}

	if truncated {
		fmt.Fprintf(stdout, "... stopped after %d nodes\n", opcua.BrowseMaxNodes)
	}

	return exitOK
}

// browseNothingBelow is what browse prints, for OPC UA and for a plugin, for a
// start node without children (a variable, an empty folder), so an empty
// answer is not taken for a failure.
const browseNothingBelow = "(no node below the start node)"

// errPluginNoBrowse is what browse says of a plugin whose manifest does not
// declare browse.
const errPluginNoBrowse = "the plugin of this input does not support browse"

// errMQTTBrowseFlags and errOtherBrowseFlags are the usage errors of a flag
// given to a driver it does not belong to.
const (
	errMQTTBrowseFlags  = "--node and --depth are for OPC UA and plugin inputs, not for an mqtt input"
	errOtherBrowseFlags = "--filter and --duration are for mqtt inputs only"
)

// loadedInput returns the document and the input id when the document loads
// cleanly and has that input; otherwise found is false and the driver paths
// report whatever is wrong, in their own words.
func loadedInput(configFile, bootstrapPath, id string) (doc config.Document, boot bootstrap.File, cfg config.Input, found bool) {
	if configFile == "" && bootstrapPath == "" {
		return doc, boot, cfg, false
	}

	doc, boot, msgs, err := probeDocument(configFile, bootstrapPath)
	if err != nil || len(msgs) > 0 {
		return doc, boot, cfg, false
	}

	i := slices.IndexFunc(doc.Inputs, func(in config.Input) bool { return in.ID == id })
	if i < 0 {
		return doc, boot, cfg, false
	}

	return doc, boot, doc.Inputs[i], true
}

// browseMQTT subscribes to filter for duration and prints what the broker
// sends, topics sorted. A topic line is
// "<topic>  <n> msg  <size> B  <retained|->" followed, for a plain payload,
// by "  <preview>"; for a JSON object or list it is followed by one line per
// leaf, indented two spaces, "  <topic>|<path>  <preview>", the address being
// usable as the address of a point (a string value is quoted); past 100
// leaves one line "  ... <n> more leaves not shown" ends the list. Previews are
// at most 40 characters, escaped; the count, size and retain flag are those
// of the messages seen, the last one for size and retain. It stops after
// 1000 topics. The connection settings and the password are never printed.
// Ctrl-C ends the collection early and still prints what was collected.
// sparkplug selects the Sparkplug B listing of format sparkplug_b, which
// stops after 1000 entries.
func browseMQTT(doc config.Document, boot bootstrap.File, cfg config.Input, configFile, bootstrapPath, filter string, duration time.Duration, sparkplug bool, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge browse: "

	if err := mqtt.CheckFilter(filter); err != nil {
		fmt.Fprintf(stderr, "%s--filter: %v\n", prefix, err)
		return exitUsage
	}
	if duration < mqtt.MinBrowseDuration || duration > mqtt.MaxBrowseDuration {
		fmt.Fprintf(stderr, "%s--duration: want a duration from 1s to 5m\n", prefix)
		return exitUsage
	}

	boot, err := probeBootstrap(doc, boot, cfg.ID, configFile, bootstrapPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	log := logging.New(stderr, slog.LevelWarn)

	in, err := mqtt.New(cfg, mqttBrowseClock, mqtt.Options{
		DataDir:  boot.DataDir,
		AgentID:  boot.AgentID,
		Resolver: secrets.NewResolver(),
		Logger:   log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}
	defer func() { _ = in.Close() }()

	ctx, release := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer release()

	ctx, cancel := context.WithTimeout(ctx, duration+browseTimeout)
	defer cancel()

	var lines []string
	var truncated bool
	if sparkplug {
		var entries []mqtt.SparkplugEntry
		if entries, truncated, err = in.BrowseSparkplug(ctx, filter, duration); err != nil {
			fmt.Fprintf(stderr, "%s%v\n", prefix, err)
			return exitFailure
		}
		lines = sparkplugBrowseLines(entries)
	} else {
		var entries []mqtt.BrowseEntry
		if entries, truncated, err = in.Browse(ctx, filter, duration); err != nil {
			fmt.Fprintf(stderr, "%s%v\n", prefix, err)
			return exitFailure
		}
		lines = mqttBrowseLines(entries)
	}

	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	if len(lines) == 0 {
		fmt.Fprintln(stdout, browseNoMessage)
	}
	if truncated {
		fmt.Fprintln(stdout, mqttBrowseTruncatedLine(sparkplug))
	}

	return exitOK
}

// browseNoMessage is what an mqtt browse prints when nothing arrived.
const browseNoMessage = "(no message received)"

// mqttBrowseTruncatedLine is the line an mqtt browse that stopped early ends
// with: it counts topics under json, and nodes and devices (entries) under
// sparkplug_b.
func mqttBrowseTruncatedLine(sparkplug bool) string {
	what := "topics"
	if sparkplug {
		what = "entries"
	}

	return fmt.Sprintf("... stopped after %d %s", mqtt.BrowseMaxTopics, what)
}

// mqttBrowseLines formats entries as browseMQTT documents.
func mqttBrowseLines(entries []mqtt.BrowseEntry) []string {
	var lines []string
	for _, e := range entries {
		retained := "-"
		if e.Retained {
			retained = "retained"
		}

		line := fmt.Sprintf("%s  %d msg  %d B  %s", mqtt.Escape(e.Topic), e.Messages, e.Size, retained)
		if e.Text != "" {
			line += "  " + e.Text
		}
		lines = append(lines, line)

		for _, p := range e.Paths {
			lines = append(lines, "  "+mqtt.Escape(p.Address)+"  "+p.Preview)
		}
		if e.MorePaths > 0 {
			lines = append(lines, fmt.Sprintf("  ... %d more leaves not shown", e.MorePaths))
		}
	}

	return lines
}

// sparkplugBrowseLines formats the entries of a Sparkplug B browse: one line
// "<group>/<node>[/<device>]  <n> msg  <born|->", with "  <n> malformed"
// when a payload was not valid, then one indented line per metric of the
// births, "  <address>  <type>", sorted by address.
func sparkplugBrowseLines(entries []mqtt.SparkplugEntry) []string {
	var lines []string
	for _, e := range entries {
		key := e.Group + "/" + e.Node
		if e.Device != "" {
			key += "/" + e.Device
		}
		born := "-"
		if e.Born {
			born = "born"
		}

		line := fmt.Sprintf("%s  %d msg  %s", mqtt.Escape(key), e.Messages, born)
		if e.Malformed > 0 {
			line += fmt.Sprintf("  %d malformed", e.Malformed)
		}
		lines = append(lines, line)

		for _, m := range e.Metrics {
			lines = append(lines, "  "+mqtt.Escape(m.Address)+"  "+m.Type)
		}
	}

	return lines
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

	if len(nodes) == 0 {
		fmt.Fprintln(stdout, browseNothingBelow)
	}

	return exitOK
}

// browseLine is the printed line of e: the data type and access columns
// appear for a variable whose attributes were read, "?" standing for the
// one that was unreadable; a node whose children the server refused ends
// with "(not browsed: <status>)".
func browseLine(e opcua.BrowseEntry) string {
	line := strings.Repeat("  ", e.Depth) + e.NodeID + "  " + e.Name + "  " + e.Class
	if e.DataType != "" || e.Access != "" {
		line += "  " + cmp.Or(e.DataType, "?") + "  " + cmp.Or(e.Access, "?")
	}
	if e.Status != "" {
		line += "  (not browsed: " + e.Status + ")"
	}

	return line
}
