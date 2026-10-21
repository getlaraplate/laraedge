package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/input/opcua/opcuatest"
)

// TestMain silences the standard log package: the library and its test
// server log through it unconditionally.
func TestMain(m *testing.M) {
	opcuatest.SilenceLibraryLogs()
	os.Exit(m.Run())
}

const testNS = opcuatest.Namespace

// opcuaEnv is a document with one secure OPC UA input "plant" against a test
// server, and a bootstrap whose data_dir is fresh.
type opcuaEnv struct {
	srv       *opcuatest.Server
	config    string
	bootstrap string
	dataDir   string
}

func newOPCUAEnv(t *testing.T, opts opcuatest.Options) opcuaEnv {
	t.Helper()

	srv := opcuatest.Start(t, opts)

	return newOPCUAEnvFor(t, srv, "security_policy: Basic256Sha256, security_mode: Sign")
}

func newOPCUAEnvFor(t *testing.T, srv *opcuatest.Server, security string) opcuaEnv {
	t.Helper()

	dir := t.TempDir()
	env := opcuaEnv{
		srv:       srv,
		config:    filepath.Join(dir, "config.yaml"),
		bootstrap: filepath.Join(dir, "bootstrap.yaml"),
		dataDir:   filepath.Join(dir, "data"),
	}

	writeFile(t, env.config, `schema: laraplate-edge-config/1
inputs:
  - id: plant
    driver: opcua
    connection: {endpoint: '`+srv.URL+`', `+security+`}
    devices:
      - device: press-07
        points:
          - {signal: temp, address: "nsu=`+testNS+`;s=temp", type: float64, read: {poll: 1s}}
outputs:
  - {id: main, type: laraplate-http, url: 'http://127.0.0.1:8000', token: secret://env/TOKEN}
`)
	writeFile(t, env.bootstrap, bootstrapYAML("plant-a-edge-01", env.config, env.dataDir))

	return env
}

func (e opcuaEnv) flags() []string {
	return []string{"--config", e.config, "--bootstrap", e.bootstrap}
}

func (e opcuaEnv) trusted(thumb string) string {
	return filepath.Join(e.dataDir, "opcua", "pki", "trusted", thumb+".der")
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := runWithInput(args, strings.NewReader(stdin), &stdout, &stderr)

	return result{code, stdout.String(), stderr.String()}
}

func (e opcuaEnv) trust(t *testing.T, stdin string, extra ...string) result {
	t.Helper()

	return runCLI(t, stdin, append([]string{"opcua", "trust", "plant"}, append(e.flags(), extra...)...)...)
}

func (e opcuaEnv) trustServer(t *testing.T) {
	t.Helper()

	if r := e.trust(t, "y\n"); r.code != exitOK {
		t.Fatalf("trust: %+v", r)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func TestOPCUATrustAsksAndStoresOnYes(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "YES\r\n", " y \n", "y"} {
		t.Run(strconv.Quote(answer), func(t *testing.T) {
			env := newOPCUAEnv(t, opcuatest.Options{})

			r := env.trust(t, answer)
			if r.code != exitOK {
				t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
			}

			for _, want := range []string{
				"Subject: laraplate test server\n",
				"Thumbprint: " + env.srv.Thumbprint + "\n",
				"Expires: ",
				"Trust this certificate? [y/N] ",
				"Trusted " + env.srv.Thumbprint + "\n",
			} {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("stdout lacks %q: %q", want, r.stdout)
				}
			}

			if strings.Contains(r.stdout, "Expired") {
				t.Errorf("a valid certificate is shown as expired: %q", r.stdout)
			}

			der, err := os.ReadFile(env.trusted(env.srv.Thumbprint))
			if err != nil || !bytes.Equal(der, env.srv.Cert) {
				t.Fatalf("trusted certificate: %v, equal %v", err, bytes.Equal(der, env.srv.Cert))
			}

			if !fileExists(filepath.Join(env.dataDir, "opcua", "pki", "own", "cert.der")) {
				t.Error("the agent certificate was not prepared")
			}
		})
	}
}

func TestOPCUATrustAnythingButYesWritesNothing(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "", "no\n", "maybe\n", "yy\n", "ye\n"} {
		t.Run(strconv.Quote(answer), func(t *testing.T) {
			env := newOPCUAEnv(t, opcuatest.Options{})

			r := env.trust(t, answer)
			if r.code != exitFailure || !strings.Contains(r.stdout, "Not trusted") || strings.Contains(r.stdout, "Trusted "+env.srv.Thumbprint) {
				t.Fatalf("exit code = %d, stdout = %q; want Not trusted and exit 1", r.code, r.stdout)
			}

			if fileExists(env.trusted(env.srv.Thumbprint)) {
				t.Fatal("a certificate was trusted")
			}
		})
	}
}

func TestOPCUATrustWithTheRightThumbprintDoesNotAsk(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})

	r := env.trust(t, "", "--thumbprint", strings.ToUpper(env.srv.Thumbprint))
	if r.code != exitOK || strings.Contains(r.stdout, "Trust this certificate?") || !strings.Contains(r.stdout, "Trusted "+env.srv.Thumbprint+"\n") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", r.code, r.stdout, r.stderr)
	}

	if !fileExists(env.trusted(env.srv.Thumbprint)) {
		t.Fatal("the certificate was not trusted")
	}
}

func TestOPCUATrustWithAWrongThumbprintRefuses(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	wrong := strings.Repeat("ab", 20)

	// A yes on stdin changes nothing: the thumbprint decides.
	r := env.trust(t, "y\n", "--thumbprint", wrong)
	if r.code != exitFailure {
		t.Fatalf("exit code = %d, want %d", r.code, exitFailure)
	}

	for _, want := range []string{"want " + wrong, "server presents " + env.srv.Thumbprint} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, r.stderr)
		}
	}

	if fileExists(env.trusted(env.srv.Thumbprint)) || strings.Contains(r.stdout, "Trusted ") {
		t.Fatal("a certificate was trusted")
	}
}

func TestOPCUATrustAnAlreadyTrustedCertificateDoesNotAsk(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	env.trustServer(t)

	r := env.trust(t, "")
	if r.code != exitOK || !strings.Contains(r.stdout, "Already trusted "+env.srv.Thumbprint+"\n") || strings.Contains(r.stdout, "Trust this certificate?") {
		t.Fatalf("exit code = %d, stdout = %q", r.code, r.stdout)
	}
}

func TestOPCUATrustShowsAnExpiredCertificate(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{Expired: true})

	r := env.trust(t, "n\n")
	if !strings.Contains(r.stdout, "(Expired)") {
		t.Fatalf("stdout = %q, want the expiry flagged", r.stdout)
	}
}

func TestOPCUATrustRefusals(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	none := newOPCUAEnvFor(t, env.srv, "security_policy: None")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"policy None", append([]string{"opcua", "trust", "plant"}, none.flags()...), `input "plant" uses security_policy None`},
		{"not an opcua input", []string{"opcua", "trust", "press-line", "--config", simulatedExample, "--bootstrap", env.bootstrap}, `input "press-line" is not an opcua input`},
		{"an unknown input", append([]string{"opcua", "trust", "nope"}, env.flags()...), `no input "nope"`},
		{"no data_dir", []string{"opcua", "trust", "plant", "--config", env.config}, "data_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCLI(t, "y\n", tc.args...)
			if r.code != exitFailure || !strings.Contains(r.stderr, tc.want) {
				t.Fatalf("exit code = %d, stderr = %q; want exit 1 and %q", r.code, r.stderr, tc.want)
			}

			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
		})
	}
}

func TestOPCUATrustUsage(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})

	for name, args := range map[string][]string{
		"no subcommand":          {"opcua"},
		"unknown subcommand":     {"opcua", "revoke"},
		"no input":               append([]string{"opcua", "trust"}, env.flags()...),
		"two inputs":             append([]string{"opcua", "trust", "a", "b"}, env.flags()...),
		"no document source":     {"opcua", "trust", "plant"},
		"list with an input":     append([]string{"opcua", "trust", "--list", "plant"}, env.flags()...),
		"list with a thumbprint": append([]string{"opcua", "trust", "--list", "--thumbprint", "ab"}, env.flags()...),
		"list without a source":  {"opcua", "trust", "--list"},
		"an unknown flag":        {"opcua", "trust", "plant", "--nope"},
	} {
		if r := runCLI(t, "", args...); r.code != exitUsage {
			t.Errorf("%s: exit code = %d (stderr %q), want %d", name, r.code, r.stderr, exitUsage)
		}
	}
}

func TestOPCUATrustOfAnAbsentServerDoesNotNameTheEndpoint(t *testing.T) {
	down := newOPCUAEnvFor(t, &opcuatest.Server{URL: "opc.tcp://127.0.0.1:1"}, "security_policy: Basic256Sha256, security_mode: Sign")

	r := down.trust(t, "y\n")
	if r.code != exitFailure || !strings.Contains(r.stderr, "opcua trust: ") || strings.Contains(r.stderr, "127.0.0.1") {
		t.Fatalf("exit code = %d, stderr = %q", r.code, r.stderr)
	}
}

// serverlessCert writes a self-signed certificate with this common name into
// the trusted directory of dataDir and returns its thumbprint.
func serverlessCert(t *testing.T, dataDir, cn string) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC),
	}, &x509.Certificate{Subject: pkix.Name{CommonName: cn}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha1.Sum(der)
	thumb := hex.EncodeToString(sum[:])

	dir := filepath.Join(dataDir, "opcua", "pki", "trusted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, thumb+".der"), string(der))

	return thumb
}

func TestOPCUATrustList(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})

	r := runCLI(t, "", append([]string{"opcua", "trust", "--list"}, "--bootstrap", env.bootstrap)...)
	if r.code != exitOK || r.stdout != "" || r.stderr != "" {
		t.Fatalf("empty store: %+v", r)
	}

	zeta := serverlessCert(t, env.dataDir, "zeta")
	alpha := serverlessCert(t, env.dataDir, "alpha")
	env.trustServer(t)

	r = runCLI(t, "", "opcua", "trust", "--list", "--bootstrap", env.bootstrap, "--config", env.config)
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("list: %+v", r)
	}

	want := []string{
		alpha + "  2031-02-03T04:05:06Z  CN=alpha",
		env.srv.Thumbprint + "  ",
		zeta + "  2031-02-03T04:05:06Z  CN=zeta",
	}

	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("list = %q, want 3 lines", r.stdout)
	}

	if lines[0] != want[0] || lines[2] != want[2] {
		t.Errorf("list = %q, want alpha first and zeta last (by subject)", lines)
	}

	if !strings.HasPrefix(lines[1], want[1]) || !strings.HasSuffix(lines[1], "  CN=laraplate test server") {
		t.Errorf("middle line = %q", lines[1])
	}
}

func TestOPCUATrustListReportsDamagedFiles(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	bad := strings.Repeat("0", 40) + ".der"

	dir := filepath.Join(env.dataDir, "opcua", "pki", "trusted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, bad), "not a certificate")

	// Nothing valid: exit 1.
	r := runCLI(t, "", "opcua", "trust", "--list", "--bootstrap", env.bootstrap)
	if r.code != exitFailure || r.stdout != "" || !strings.Contains(r.stderr, bad) {
		t.Fatalf("only a damaged file: %+v", r)
	}

	// A valid certificate beside it is listed; the damage is still reported.
	good := serverlessCert(t, env.dataDir, "good")

	r = runCLI(t, "", "opcua", "trust", "--list", "--bootstrap", env.bootstrap)
	if r.code != exitOK || !strings.HasPrefix(r.stdout, good+"  ") || !strings.Contains(r.stderr, bad) {
		t.Fatalf("a damaged and a valid file: %+v", r)
	}
}

func TestBrowseOfAnUntrustedServerPrintsTheThumbprintAndTheCommand(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})

	r := runCLI(t, "", append([]string{"browse", "plant"}, env.flags()...)...)
	if r.code != exitFailure || r.stdout != "" {
		t.Fatalf("exit code = %d, stdout = %q", r.code, r.stdout)
	}

	for _, want := range []string{env.srv.Thumbprint, "opcua trust plant"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, r.stderr)
		}
	}

	if strings.Contains(r.stderr, "127.0.0.1") {
		t.Errorf("stderr names the endpoint: %q", r.stderr)
	}
}

func browseLines(t *testing.T, env opcuaEnv, args ...string) []string {
	t.Helper()

	r := runCLI(t, "", append(append([]string{"browse", "plant"}, env.flags()...), args...)...)
	if r.code != exitOK || r.stderr != "" {
		t.Fatalf("browse %v: exit code = %d, stderr = %q", args, r.code, r.stderr)
	}

	return strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
}

func TestBrowseFromANodeIndentsByDepthAndNamesDataTypes(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	env.trustServer(t)

	prefix := "nsu=" + testNS + ";"
	line1 := prefix + "s=Line1"

	got := browseLines(t, env, "--node", line1, "--depth", "3")
	want := []string{
		prefix + "s=speed  speed  Variable  Double",
		prefix + "s=state  state  Variable  String",
		prefix + "s=Cell  Cell  Object",
		"  " + prefix + "s=deep  deep  Variable  Boolean",
		// The reference back to the start node is printed once, and what is
		// below it was printed already.
		"  " + line1 + "  Line1  Object",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("browse =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Depth 1 prints the children of the start node only; the default is 2.
	if got := browseLines(t, env, "--node", line1, "--depth", "1"); len(got) != 3 {
		t.Fatalf("depth 1 = %q, want 3 lines", got)
	}

	if got := browseLines(t, env, "--node", line1); len(got) != 5 || !strings.HasPrefix(got[3], "  ") {
		t.Fatalf("default depth = %q, want 5 lines (two levels)", got)
	}

	// A plain NodeId with the namespace index works too.
	idx := strconv.Itoa(int(env.srv.NamespaceIndex))
	if got := browseLines(t, env, "--node", "ns="+idx+";s=Line1", "--depth", "1"); len(got) != 3 {
		t.Fatalf("--node ns=%s;s=Line1 = %q", idx, got)
	}
}

func TestBrowseStartsAtTheObjectsFolder(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	env.trustServer(t)

	got := strings.Join(browseLines(t, env, "--depth", "1"), "\n")

	for _, want := range []string{"i=2253  Server  Object", "nsu=" + testNS + ";i=85  "} {
		if !strings.Contains(got, want) {
			t.Errorf("browse lacks %q:\n%s", want, got)
		}
	}
}

func TestBrowseStopsAfterThousandNodes(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{Bulk: 1005})
	env.trustServer(t)

	got := browseLines(t, env, "--node", "nsu="+testNS+";s=Bulk", "--depth", "1")
	if len(got) != 1001 {
		t.Fatalf("%d lines, want 1000 nodes and the note", len(got))
	}

	if got[0] != "nsu="+testNS+";s=v0  v0  Variable  Int32" || got[1000] != "... stopped after 1000 nodes" {
		t.Fatalf("first line %q, last line %q", got[0], got[1000])
	}
}

func TestBrowseFailures(t *testing.T) {
	env := newOPCUAEnv(t, opcuatest.Options{})
	env.trustServer(t)

	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"depth 0", []string{"--depth", "0"}, exitUsage},
		{"depth 11", []string{"--depth", "11"}, exitUsage},
		{"a bad node id", []string{"--node", "Line1"}, exitUsage},
		{"an unknown namespace", []string{"--node", "nsu=urn:nope;s=x"}, exitFailure},
		{"an unknown node", []string{"--node", "ns=" + strconv.Itoa(int(env.srv.NamespaceIndex)) + ";s=nope"}, exitFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCLI(t, "", append(append([]string{"browse", "plant"}, env.flags()...), tc.args...)...)
			if r.code != tc.code || r.stdout != "" || r.stderr == "" || strings.Contains(r.stderr, "127.0.0.1") {
				t.Fatalf("%+v; want exit %d, no stdout and a message without the endpoint", r, tc.code)
			}
		})
	}

	for name, args := range map[string][]string{
		"no input":           append([]string{"browse"}, env.flags()...),
		"no document source": {"browse", "plant"},
	} {
		if r := runCLI(t, "", args...); r.code != exitUsage {
			t.Errorf("%s: exit code = %d, want %d", name, r.code, exitUsage)
		}
	}

	if r := runCLI(t, "", "browse", "press-line", "--config", simulatedExample, "--bootstrap", env.bootstrap); r.code != exitFailure || !strings.Contains(r.stderr, "not an opcua input") {
		t.Errorf("a simulated input: %+v", r)
	}
}
