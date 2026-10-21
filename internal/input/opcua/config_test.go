package opcua

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"

	"github.com/getlaraplate/laraedge/internal/config"
)

func TestParseConnection(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Connection
	}{
		{
			name: "defaults",
			raw:  `{"endpoint":"opc.tcp://10.0.5.20:4840"}`,
			want: Connection{Endpoint: "opc.tcp://10.0.5.20:4840", SecurityPolicy: "Basic256Sha256", SecurityMode: "SignAndEncrypt", PublishingInterval: time.Second},
		},
		{
			name: "spec example",
			raw:  `{"endpoint":"opc.tcp://10.0.5.20:4840","security_policy":"Basic256Sha256","auth":{"user":"mes","password":"secret://keystore/opcua-press"}}`,
			want: Connection{Endpoint: "opc.tcp://10.0.5.20:4840", SecurityPolicy: "Basic256Sha256", SecurityMode: "SignAndEncrypt", User: "mes", Password: "secret://keystore/opcua-press", PublishingInterval: time.Second},
		},
		{
			name: "Aes128 and Sign",
			raw:  `{"endpoint":"opc.tcp://plc","security_policy":"Aes128_Sha256_RsaOaep","security_mode":"Sign"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "Aes128_Sha256_RsaOaep", SecurityMode: "Sign", PublishingInterval: time.Second},
		},
		{
			name: "Aes256 and SignAndEncrypt",
			raw:  `{"endpoint":"opc.tcp://plc","security_policy":"Aes256_Sha256_RsaPss","security_mode":"SignAndEncrypt"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "Aes256_Sha256_RsaPss", SecurityMode: "SignAndEncrypt", PublishingInterval: time.Second},
		},
		{
			name: "Basic256Sha256 and Sign",
			raw:  `{"endpoint":"opc.tcp://plc","security_policy":"Basic256Sha256","security_mode":"Sign"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "Basic256Sha256", SecurityMode: "Sign", PublishingInterval: time.Second},
		},
		{
			name: "None without a mode",
			raw:  `{"endpoint":"opc.tcp://plc","security_policy":"None"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "None", SecurityMode: "None", PublishingInterval: time.Second},
		},
		{
			name: "None ignores a mode",
			raw:  `{"endpoint":"opc.tcp://plc","security_policy":"None","security_mode":"Sign"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "None", SecurityMode: "None", PublishingInterval: time.Second},
		},
		{
			name: "anonymous string",
			raw:  `{"endpoint":"opc.tcp://plc","auth":"anonymous"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "Basic256Sha256", SecurityMode: "SignAndEncrypt", PublishingInterval: time.Second},
		},
		{
			name: "publishing interval",
			raw:  `{"endpoint":"opc.tcp://plc","publishing_interval":"250ms"}`,
			want: Connection{Endpoint: "opc.tcp://plc", SecurityPolicy: "Basic256Sha256", SecurityMode: "SignAndEncrypt", PublishingInterval: 250 * time.Millisecond},
		},
		{
			name: "host name endpoint without a port",
			raw:  `{"endpoint":"opc.tcp://server.plant.example"}`,
			want: Connection{Endpoint: "opc.tcp://server.plant.example", SecurityPolicy: "Basic256Sha256", SecurityMode: "SignAndEncrypt", PublishingInterval: time.Second},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConnection(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("ParseConnection: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseConnectionRefusals(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, "connection: must be an object"},
		{"null", `null`, "connection: must be an object"},
		{"array", `[]`, "connection: must be an object"},
		{"missing endpoint", `{}`, "connection.endpoint: required"},
		{"empty endpoint", `{"endpoint":""}`, "connection.endpoint: required"},
		{"endpoint not string", `{"endpoint":4840}`, "connection.endpoint: must be a string"},
		{"endpoint wrong scheme", `{"endpoint":"http://plc:4840"}`, "connection.endpoint: want opc.tcp://<host>[:<port>]"},
		{"endpoint upper-case scheme", `{"endpoint":"OPC.TCP://plc:4840"}`, "connection.endpoint: want opc.tcp://<host>[:<port>]"},
		{"endpoint without a scheme", `{"endpoint":"plc:4840"}`, "connection.endpoint: want opc.tcp://<host>[:<port>]"},
		{"endpoint without a host", `{"endpoint":"opc.tcp://"}`, "connection.endpoint: want opc.tcp://<host>[:<port>]"},
		{"endpoint with a blank host", `{"endpoint":"opc.tcp://:4840"}`, "connection.endpoint: want opc.tcp://<host>[:<port>]"},
		{"endpoint with credentials", `{"endpoint":"opc.tcp://user:pw@plc:4840"}`, "connection.endpoint: must not contain credentials"},
		{"policy unknown", `{"endpoint":"opc.tcp://plc","security_policy":"Basic128Rsa15"}`, "connection.security_policy: want None, Basic256Sha256, Aes128_Sha256_RsaOaep or Aes256_Sha256_RsaPss"},
		{"policy lower case", `{"endpoint":"opc.tcp://plc","security_policy":"none"}`, "connection.security_policy: want None, Basic256Sha256, Aes128_Sha256_RsaOaep or Aes256_Sha256_RsaPss"},
		{"policy empty", `{"endpoint":"opc.tcp://plc","security_policy":""}`, "connection.security_policy: want None, Basic256Sha256, Aes128_Sha256_RsaOaep or Aes256_Sha256_RsaPss"},
		{"policy not string", `{"endpoint":"opc.tcp://plc","security_policy":1}`, "connection.security_policy: must be a string"},
		{"mode unknown", `{"endpoint":"opc.tcp://plc","security_mode":"Encrypt"}`, "connection.security_mode: want Sign or SignAndEncrypt"},
		{"mode None", `{"endpoint":"opc.tcp://plc","security_mode":"None"}`, "connection.security_mode: want Sign or SignAndEncrypt"},
		{"mode unknown with None policy", `{"endpoint":"opc.tcp://plc","security_policy":"None","security_mode":"Encrypt"}`, "connection.security_mode: want Sign or SignAndEncrypt"},
		{"mode not string", `{"endpoint":"opc.tcp://plc","security_mode":true}`, "connection.security_mode: must be a string"},
		{"auth unknown string", `{"endpoint":"opc.tcp://plc","auth":"basic"}`, `connection.auth: want "anonymous" or {user, password}`},
		{"auth number", `{"endpoint":"opc.tcp://plc","auth":1}`, `connection.auth: want "anonymous" or {user, password}`},
		{"auth null", `{"endpoint":"opc.tcp://plc","auth":null}`, `connection.auth: want "anonymous" or {user, password}`},
		{"auth array", `{"endpoint":"opc.tcp://plc","auth":[]}`, `connection.auth: want "anonymous" or {user, password}`},
		{"auth without user", `{"endpoint":"opc.tcp://plc","auth":{"password":"secret://env/P"}}`, "connection.auth.user: required"},
		{"auth with empty user", `{"endpoint":"opc.tcp://plc","auth":{"user":"","password":"secret://env/P"}}`, "connection.auth.user: required"},
		{"auth user not string", `{"endpoint":"opc.tcp://plc","auth":{"user":1,"password":"secret://env/P"}}`, "connection.auth.user: must be a string"},
		{"auth without password", `{"endpoint":"opc.tcp://plc","auth":{"user":"mes"}}`, "connection.auth.password: required"},
		{"auth password not string", `{"endpoint":"opc.tcp://plc","auth":{"user":"mes","password":1}}`, "connection.auth.password: must be a string"},
		{"auth plain password", `{"endpoint":"opc.tcp://plc","auth":{"user":"mes","password":"hunter2"}}`, "connection.auth.password: must be a secret:// reference"},
		{"auth empty reference", `{"endpoint":"opc.tcp://plc","auth":{"user":"mes","password":"secret://"}}`, "connection.auth.password: must be a secret:// reference"},
		{"auth unknown field", `{"endpoint":"opc.tcp://plc","auth":{"user":"mes","password":"secret://env/P","token":"x"}}`, "connection.auth.token: unknown field"},
		{"user with None", `{"endpoint":"opc.tcp://plc","security_policy":"None","auth":{"user":"mes","password":"secret://env/P"}}`, "connection.auth: user and password need a security_policy other than None"},
		{"interval not a duration", `{"endpoint":"opc.tcp://plc","publishing_interval":"soon"}`, "connection.publishing_interval: want a duration such as 1s"},
		{"interval number", `{"endpoint":"opc.tcp://plc","publishing_interval":1}`, "connection.publishing_interval: want a duration such as 1s"},
		{"interval zero", `{"endpoint":"opc.tcp://plc","publishing_interval":"0s"}`, "connection.publishing_interval: must be at least 1ms"},
		{"interval negative", `{"endpoint":"opc.tcp://plc","publishing_interval":"-1s"}`, "connection.publishing_interval: must be at least 1ms"},
		{"interval below a millisecond", `{"endpoint":"opc.tcp://plc","publishing_interval":"500us"}`, "connection.publishing_interval: must be at least 1ms"},
		{"unknown field", `{"endpoint":"opc.tcp://plc","timeout":"5s"}`, "connection.timeout: unknown field"},
		{"unknown field is named before other checks", `{"endpoint":"x","retries":3}`, "connection.retries: unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnection(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseConnectionErrorsNeverEchoValues(t *testing.T) {
	secrets := []string{"SECRET-HOST", "SECRET-USER", "SECRET-PASSWORD", "SECRET-POLICY", "SECRET-MODE", "SECRET-INTERVAL", "SECRET-AUTH"}
	raws := []string{
		`{"endpoint":"SECRET-HOST"}`,
		`{"endpoint":"http://SECRET-HOST"}`,
		`{"endpoint":"opc.tcp://SECRET-USER:SECRET-PASSWORD@SECRET-HOST"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","security_policy":"SECRET-POLICY"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","security_mode":"SECRET-MODE"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","auth":"SECRET-AUTH"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","auth":{"user":"SECRET-USER","password":"SECRET-PASSWORD"}}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","auth":{"user":"SECRET-USER","password":1}}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","security_policy":"None","auth":{"user":"SECRET-USER","password":"secret://env/SECRET-PASSWORD"}}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","publishing_interval":"SECRET-INTERVAL"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","publishing_interval":"-1s"}`,
		`{"endpoint":"opc.tcp://SECRET-HOST","auth":{"SECRET-USER":"x"}}`,
	}
	for _, raw := range raws {
		_, err := ParseConnection(json.RawMessage(raw))
		if err == nil {
			t.Fatalf("%s: expected an error", raw)
		}
		for _, s := range secrets {
			// An unknown field names its key by contract; the key of the last case is a
			// field name, not a value.
			if strings.Contains(err.Error(), s) && !strings.HasSuffix(err.Error(), ": unknown field") {
				t.Fatalf("%s: error %q echoes a value", raw, err.Error())
			}
		}
	}
}

func TestParseNode(t *testing.T) {
	const guid = "09087e75-8e5e-499b-954f-f2a9603db28a"
	tests := []struct {
		name string
		in   string
		want func(t *testing.T, n Node)
	}{
		{"string id", "ns=3;s=Machine.State", func(t *testing.T, n Node) {
			wantID(t, n, ua.NewStringNodeID(3, "Machine.State"))
		}},
		{"string id with semicolon", "ns=3;s=a;b", func(t *testing.T, n Node) {
			wantID(t, n, ua.NewStringNodeID(3, "a;b"))
		}},
		{"numeric id without a namespace", "i=2258", func(t *testing.T, n Node) {
			if n.ID == nil || n.ID.Namespace() != 0 || n.ID.IntID() != 2258 {
				t.Fatalf("node = %+v", n)
			}
		}},
		{"numeric id large", "ns=300;i=70000", func(t *testing.T, n Node) {
			if n.ID == nil || n.ID.Namespace() != 300 || n.ID.IntID() != 70000 {
				t.Fatalf("node = %+v", n)
			}
		}},
		{"string id without a namespace", "s=Counter", func(t *testing.T, n Node) {
			wantID(t, n, ua.NewStringNodeID(0, "Counter"))
		}},
		{"guid id", "ns=2;g=" + guid, func(t *testing.T, n Node) {
			if n.ID == nil || n.ID.Namespace() != 2 || n.ID.Type() != ua.NodeIDTypeGUID {
				t.Fatalf("node = %+v", n)
			}
		}},
		{"opaque id", "ns=2;b=AQIDBA==", func(t *testing.T, n Node) {
			wantID(t, n, ua.NewByteStringNodeID(2, []byte{1, 2, 3, 4}))
		}},
		{"namespace uri", "nsu=urn:x;s=a;b", func(t *testing.T, n Node) {
			if n.ID != nil || n.NamespaceURI != "urn:x" || n.Rest != "s=a;b" {
				t.Fatalf("node = %+v", n)
			}
		}},
		{"namespace uri with a numeric id", "nsu=http://example.com/UA/;i=17", func(t *testing.T, n Node) {
			if n.ID != nil || n.NamespaceURI != "http://example.com/UA/" || n.Rest != "i=17" {
				t.Fatalf("node = %+v", n)
			}
		}},
		{"namespace index at the limit", "ns=65535;i=1", func(t *testing.T, n Node) {
			if n.ID == nil || n.ID.Namespace() != 65535 {
				t.Fatalf("node = %+v", n)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseNode(tc.in)
			if err != nil {
				t.Fatalf("ParseNode: %v", err)
			}
			tc.want(t, n)
		})
	}
}

func wantID(t *testing.T, n Node, want *ua.NodeID) {
	t.Helper()

	if n.ID == nil || n.NamespaceURI != "" || n.Rest != "" {
		t.Fatalf("node = %+v", n)
	}
	if n.ID.String() != want.String() {
		t.Fatalf("id = %s, want %s", n.ID, want)
	}
}

func TestParseNodeRefusals(t *testing.T) {
	const shape = "address: want ns=<n>;<i|s|g|b>=<id>, <i|s|g|b>=<id> or nsu=<uri>;<i|s|g|b>=<id>"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "address: required"},
		{"namespace uri without an identifier", "nsu=urn:x", shape},
		{"namespace uri with an empty rest", "nsu=urn:x;", shape},
		{"namespace uri with a blank uri", "nsu=;s=a", "address: namespace uri: required"},
		{"bare namespace uri prefix", "nsu=", shape},
		{"namespace uri with a namespace index", "nsu=urn:x;ns=2;s=a", shape},
		{"namespace uri with a bad numeric id", "nsu=urn:x;i=abc", "address: identifier: invalid"},
		{"namespace uri with a bad guid", "nsu=urn:x;g=zzz", "address: identifier: invalid"},
		{"namespace index out of range", "ns=70000;i=1", "address: namespace index: want an integer from 0 to 65535"},
		{"namespace index negative", "ns=-1;i=1", "address: namespace index: want an integer from 0 to 65535"},
		{"namespace index signed", "ns=+2;i=1", "address: namespace index: want an integer from 0 to 65535"},
		{"namespace index blank", "ns=;i=1", "address: namespace index: want an integer from 0 to 65535"},
		{"namespace index text", "ns=two;i=1", "address: namespace index: want an integer from 0 to 65535"},
		{"namespace without an identifier", "ns=3", shape},
		{"namespace with an empty identifier", "ns=3;", shape},
		{"bare name", "Machine.State", shape},
		{"unknown identifier type", "ns=2;x=1", shape},
		{"upper-case identifier type", "ns=2;S=a", shape},
		{"empty string id", "ns=2;s=", shape},
		{"numeric id not a number", "ns=2;i=abc", "address: identifier: invalid"},
		{"numeric id negative", "ns=2;i=-1", "address: identifier: invalid"},
		{"numeric id beyond 32 bits", "ns=2;i=4294967296", "address: identifier: invalid"},
		{"numeric id empty", "i=", shape},
		{"guid malformed", "ns=2;g=nope", "address: identifier: invalid"},
		{"opaque id malformed", "ns=2;b=!!!", "address: identifier: invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseNode(tc.in)
			if err == nil {
				t.Fatalf("accepted: %+v", n)
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
			if !reflect.DeepEqual(n, Node{}) {
				t.Fatalf("node not zero with an error: %+v", n)
			}
		})
	}
}

func TestParseNodeErrorsNeverEchoValues(t *testing.T) {
	for _, in := range []string{
		"SECRET-NODE", "ns=SECRET-NODE;i=1", "ns=2;i=SECRET-NODE", "ns=2;g=SECRET-NODE", "ns=2;b=SECRET-NODE", "nsu=SECRET-NODE",
		"nsu=;s=SECRET-NODE", "nsu=SECRET-NODE;i=SECRET-NODE", "ns=99999;s=SECRET-NODE", "ns=2;x=SECRET-NODE",
	} {
		_, err := ParseNode(in)
		if err == nil {
			t.Fatalf("%q: expected an error", in)
		}
		if strings.Contains(err.Error(), "SECRET-NODE") {
			t.Fatalf("%q: error %q echoes the value", in, err.Error())
		}
	}
}

func FuzzParseNode(f *testing.F) {
	for _, seed := range []string{"", "i=2258", "ns=3;s=Machine.State", "nsu=urn:x;s=a;b", "ns=2;g=09087e75-8e5e-499b-954f-f2a9603db28a", "ns=2;b=AQID", "ns=70000;i=1", "nsu=", ";", "ns=;"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseNode(s)
		switch {
		case err != nil && !reflect.DeepEqual(n, Node{}):
			t.Fatalf("node not zero with an error: %+v", n)
		case err == nil && n.ID == nil && n.NamespaceURI == "":
			t.Fatalf("accepted without an id or a namespace uri: %+v", n)
		case err == nil && n.ID != nil:
			_ = n.ID.String()
		}
	})
}

func TestParseDeadband(t *testing.T) {
	absolute, percent := uint32(ua.DeadbandTypeAbsolute), uint32(ua.DeadbandTypePercent)
	tests := []struct {
		in   string
		kind uint32
		want float64
	}{
		{"", uint32(ua.DeadbandTypeNone), 0},
		{"0", absolute, 0},
		{"0.5", absolute, 0.5},
		{"3", absolute, 3},
		{"1000000", absolute, 1000000},
		{"2%", percent, 2},
		{"0%", percent, 0},
		{"12.5%", percent, 12.5},
		{"100%", percent, 100},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			kind, value, err := ParseDeadband(tc.in)
			if err != nil {
				t.Fatalf("ParseDeadband: %v", err)
			}
			if kind != tc.kind || value != tc.want {
				t.Fatalf("got (%d, %v), want (%d, %v)", kind, value, tc.kind, tc.want)
			}
		})
	}
}

func TestParseDeadbandRefusals(t *testing.T) {
	const want = "deadband: want a non-negative number or N% up to 100"
	for _, in := range []string{
		"x", "-1", "-1%", "101%", "100.5%", "%", "1%%", ".5", "5.", "1e3", "+1", "0x10", "Inf", "NaN", "1 ", " 1", "1,5",
		"1" + strings.Repeat("0", 400),
	} {
		kind, value, err := ParseDeadband(in)
		if err == nil {
			t.Fatalf("%q: accepted as (%d, %v)", in, kind, value)
		}
		if err.Error() != want {
			t.Fatalf("%q: got %q, want %q", in, err.Error(), want)
		}
		if kind != 0 || value != 0 {
			t.Fatalf("%q: result not zero with an error: (%d, %v)", in, kind, value)
		}
	}
}

func point(typ, byteOrder, mode string) config.Point {
	p := config.Point{Signal: "s", Type: typ, ByteOrder: byteOrder, Read: config.Read{Mode: mode}}
	switch mode {
	case config.ReadPoll:
		p.Read.Interval = time.Second
	case config.ReadSubscribe:
		p.Read.Subscribe = &config.Subscribe{Sampling: config.SamplingDefault}
	}
	return p
}

func TestCheckPointAccepts(t *testing.T) {
	for _, typ := range []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"} {
		for _, mode := range []string{config.ReadPoll, config.ReadSubscribe} {
			t.Run(typ+" "+mode, func(t *testing.T) {
				if err := CheckPoint(point(typ, "", mode)); err != nil {
					t.Fatalf("CheckPoint: %v", err)
				}
			})
		}
	}
}

func TestCheckPointRefusals(t *testing.T) {
	const typeWant = "type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string"
	tests := []struct {
		name string
		p    config.Point
		want string
	}{
		{"missing type", point("", "", config.ReadPoll), "type: required"},
		{"unknown type", point("decimal", "", config.ReadSubscribe), typeWant},
		{"int8 is not a type", point("int8", "", config.ReadPoll), typeWant},
		{"byte order", point("int32", "ABCD", config.ReadPoll), "byte_order: not for opcua"},
		{"byte order on a string", point("string", "DCBA", config.ReadSubscribe), "byte_order: not for opcua"},
		{"event", point("int32", "", config.ReadEvent), "read: want subscribe or poll"},
		{"no read mode", point("int32", "", ""), "read: want subscribe or poll"},
		{"unknown read mode", point("int32", "", "sometimes"), "read: want subscribe or poll"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPoint(tc.p)
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Error() != tc.want {
				t.Fatalf("got %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestCheckPointRefusesABadSubscribeDeadband(t *testing.T) {
	p := point("float64", "", config.ReadSubscribe)
	p.Read.Subscribe.Deadband = "101%"
	if err := CheckPoint(p); err == nil || err.Error() != "deadband: want a non-negative number or N% up to 100" {
		t.Fatalf("CheckPoint = %v", err)
	}

	p.Read.Subscribe.Deadband = "2%"
	if err := CheckPoint(p); err != nil {
		t.Fatalf("CheckPoint = %v", err)
	}
}
