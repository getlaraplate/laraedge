package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/getlaraplate/laraedge/internal/bootstrap"
	"github.com/getlaraplate/laraedge/internal/config"
	"github.com/getlaraplate/laraedge/internal/input/opcua"
	"github.com/getlaraplate/laraedge/internal/logging"
)

// trustTimeout bounds the discovery of the server's endpoints.
const trustTimeout = 30 * time.Second

// opcuaTarget is an OPC UA input of the document with the bootstrap that
// gives its data directory and agent id.
type opcuaTarget struct {
	input config.Input
	boot  bootstrap.File
}

// openOPCUAInput finds the OPC UA input id of the document named by --config
// or --bootstrap, like probe does (the data_dir and agent_id of an opcua
// input always come from the bootstrap, ruling R9). On failure it has
// printed the message and returns the exit code.
func openOPCUAInput(command string, stderr io.Writer, configFile, bootstrapPath, id string) (opcuaTarget, int, bool) {
	prefix := "laraplate-edge " + command + ": "

	if configFile == "" && bootstrapPath == "" {
		fmt.Fprintf(stderr, "%s--config <file> or --bootstrap <path> is required\n", prefix)
		return opcuaTarget{}, exitUsage, false
	}

	doc, boot, msgs, err := probeDocument(configFile, bootstrapPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return opcuaTarget{}, exitFailure, false
	}
	if len(msgs) > 0 {
		printMessages(stderr, configFile, msgs)
		return opcuaTarget{}, exitFailure, false
	}

	if boot, err = probeBootstrap(doc, boot, id, configFile, bootstrapPath); err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return opcuaTarget{}, exitFailure, false
	}

	i := slices.IndexFunc(doc.Inputs, func(in config.Input) bool { return in.ID == id })
	if i < 0 {
		fmt.Fprintf(stderr, "%sthe configuration has no input %q (inputs: %s)\n", prefix, id, inputIDs(doc))
		return opcuaTarget{}, exitFailure, false
	}
	if doc.Inputs[i].Driver != config.DriverOPCUA {
		fmt.Fprintf(stderr, "%sinput %q is not an opcua input\n", prefix, id)
		return opcuaTarget{}, exitFailure, false
	}

	if boot.DataDir == "" {
		fmt.Fprintf(stderr, "%sdata_dir unknown: give --bootstrap, the agent certificate and the trusted certificates live there\n", prefix)
		return opcuaTarget{}, exitFailure, false
	}

	return opcuaTarget{input: doc.Inputs[i], boot: boot}, exitOK, true
}

// runOPCUA runs "opcua trust".
func runOPCUA(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "trust" {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	return runOPCUATrust(args[1:], stdin, stdout, stderr)
}

// runOPCUATrust runs "opcua trust <input>" and "opcua trust --list": the
// explicit trust of the certificate an OPC UA server presents (decision O1).
// The certificate comes from the server's endpoint discovery only: no
// session is opened and nothing is read. It is stored in trusted/ only when
// the operator confirms it on stdin or gives its thumbprint.
func runOPCUATrust(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge opcua trust: "

	fs := newFlagSet("opcua trust", stderr)
	bootstrapPath := fs.String("bootstrap", "", "path to the bootstrap file; its data_dir holds the PKI and the last valid document")
	configFile := fs.String("config", "", "configuration document to use instead of the last valid one")
	thumbprint := fs.String("thumbprint", "", "trust without asking when the server presents this thumbprint")
	list := fs.Bool("list", false, "list the trusted certificates")

	positional, code, ok := parseArgs(fs, args)
	if !ok {
		return code
	}

	if *list {
		if len(positional) != 0 || *thumbprint != "" {
			fmt.Fprint(stderr, usageText)
			return exitUsage
		}

		return listTrusted(*bootstrapPath, *configFile, stdout, stderr)
	}

	if len(positional) != 1 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}

	target, code, ok := openOPCUAInput("opcua trust", stderr, *configFile, *bootstrapPath, positional[0])
	if !ok {
		return code
	}

	conn, err := opcua.ParseConnection(target.input.Connection)
	if err != nil {
		fmt.Fprintf(stderr, "%sinput %q: %v\n", prefix, target.input.ID, err)
		return exitFailure
	}
	if conn.SecurityPolicy == opcua.PolicyNone {
		fmt.Fprintf(stderr, "%sinput %q uses security_policy None: there is no certificate to trust\n", prefix, target.input.ID)
		return exitFailure
	}

	log := logging.New(stderr, slog.LevelWarn)
	defer routeLibraryLogs(log)()

	if _, _, err := opcua.EnsureOwnCertificate(target.boot.DataDir, target.boot.AgentID, time.Now()); err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	ctx, cancel := context.WithTimeout(context.Background(), trustTimeout)
	defer cancel()

	cert, err := opcua.DiscoverServerCertificate(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "%sinput %q: %v\n", prefix, target.input.ID, err)
		return exitFailure
	}

	expires := cert.NotAfter.UTC().Format(time.RFC3339)
	if cert.NotAfter.Before(time.Now()) {
		expires += " (Expired)"
	}

	fmt.Fprintf(stdout, "Subject: %s\nThumbprint: %s\nExpires: %s\n", cert.Subject, cert.Thumbprint, expires)

	if *thumbprint != "" && !strings.EqualFold(*thumbprint, cert.Thumbprint) {
		fmt.Fprintf(stderr, "%sthumbprint mismatch: want %s, server presents %s\n", prefix, *thumbprint, cert.Thumbprint)
		return exitFailure
	}

	store := opcua.TrustStore{Dir: opcua.PKIDir(target.boot.DataDir)}

	if store.Trusted(cert.Thumbprint) {
		fmt.Fprintf(stdout, "Already trusted %s\n", cert.Thumbprint)
		return exitOK
	}

	if *thumbprint == "" && !confirm(stdin, stdout) {
		fmt.Fprintln(stdout, "Not trusted")
		return exitFailure
	}

	stored, err := store.Trust(cert.DER)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}
	if stored != cert.Thumbprint {
		fmt.Fprintf(stderr, "%sthe stored certificate has another thumbprint than the one shown\n", prefix)
		return exitFailure
	}

	fmt.Fprintf(stdout, "Trusted %s\n", cert.Thumbprint)

	return exitOK
}

// confirm asks "Trust this certificate? [y/N]" and reads one line from in:
// only y or yes, in any case, is a yes; the end of the input is a no.
func confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Trust this certificate? [y/N] ")

	line, err := bufio.NewReader(io.LimitReader(in, 64)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// listTrusted prints "<thumbprint>  <expires>  <subject>" for each trusted
// certificate, by subject then thumbprint. Unreadable files are named on
// stderr; the exit code is 1 only when some file is unreadable and no
// certificate could be listed.
func listTrusted(bootstrapPath, configFile string, stdout, stderr io.Writer) int {
	const prefix = "laraplate-edge opcua trust: "

	if bootstrapPath == "" {
		if configFile == "" {
			fmt.Fprintf(stderr, "%s--config <file> or --bootstrap <path> is required\n", prefix)
			return exitUsage
		}

		fmt.Fprintf(stderr, "%sdata_dir unknown: give --bootstrap, the trusted certificates live there\n", prefix)

		return exitFailure
	}

	boot, err := bootstrap.Load(bootstrapPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return exitFailure
	}

	infos, err := opcua.TrustStore{Dir: opcua.PKIDir(boot.DataDir)}.List()

	slices.SortFunc(infos, func(a, b opcua.CertInfo) int {
		return strings.Compare(a.Subject+"\x00"+a.Thumbprint, b.Subject+"\x00"+b.Thumbprint)
	})

	for _, c := range infos {
		fmt.Fprintf(stdout, "%s  %s  %s\n", c.Thumbprint, c.NotAfter.UTC().Format(time.RFC3339), opcua.Printable(c.Subject))
	}

	if err != nil {
		for line := range strings.SplitSeq(err.Error(), "\n") {
			fmt.Fprintf(stderr, "%s%s\n", prefix, line)
		}

		if len(infos) == 0 {
			return exitFailure
		}
	}

	return exitOK
}
