package mqtt

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getlaraplate/laraedge/internal/config"
)

// testInput is an input whose devices each have one point on topic plant/a.
func testInput(devices ...string) config.Input {
	in := config.Input{ID: "broker", Driver: config.DriverMQTT}
	for _, d := range devices {
		in.Devices = append(in.Devices, config.Device{Device: d, Points: []config.Point{
			{Signal: "v", Address: "plant/a|" + d, Type: "float64"},
		}})
	}
	return in
}

func TestParseConnection(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		devices []string
		check   func(t *testing.T, c Connection)
	}{
		{
			name: "defaults",
			raw:  `{"broker":"mqtt://broker.local"}`,
			check: func(t *testing.T, c Connection) {
				if c.Broker.String() != "mqtt://broker.local:1883" {
					t.Errorf("broker = %q", c.Broker)
				}
				if c.ClientID != "" || c.Auth != nil || c.TLS != nil || c.Timestamp != nil {
					t.Errorf("unexpected optional settings: %+v", c)
				}
				if c.Format != "json" || c.QoS != 1 || !c.PersistentSession || c.KeepAlive != 30*time.Second || c.MaxPayload != 262144 {
					t.Errorf("defaults wrong: %+v", c)
				}
				if len(c.Topics) != 0 || len(c.Availability) != 0 {
					t.Errorf("topics/availability should be empty: %+v", c)
				}
			},
		},
		{
			name: "mqtts default port",
			raw:  `{"broker":"mqtts://broker.local"}`,
			check: func(t *testing.T, c Connection) {
				if c.Broker.String() != "mqtts://broker.local:8883" {
					t.Errorf("broker = %q", c.Broker)
				}
			},
		},
		{
			name: "explicit port kept",
			raw:  `{"broker":"mqtt://10.0.0.5:1999"}`,
			check: func(t *testing.T, c Connection) {
				if c.Broker.Host != "10.0.0.5:1999" {
					t.Errorf("host = %q", c.Broker.Host)
				}
			},
		},
		{
			name: "ipv6 host gets the default port",
			raw:  `{"broker":"mqtt://[::1]"}`,
			check: func(t *testing.T, c Connection) {
				if c.Broker.Host != "[::1]:1883" {
					t.Errorf("host = %q", c.Broker.Host)
				}
			},
		},
		{
			name: "full document",
			raw: `{"broker":"mqtts://b:8884","client_id":"edge-1","auth":{"user":"u","password":"secret://keystore/mqtt"},
				"tls":{"ca_file":"/ca.pem","cert_file":"/c.pem","key_file":"/k.pem","server_name":"b.example"},
				"format":"json","qos":0,"persistent_session":false,"keepalive":"10s","max_payload":"1MiB",
				"timestamp":{"path":"meta.ts","format":"unix_ms"},
				"topics":{"plant/a":{"timestamp":{"path":"t","format":"rfc3339"}}},
				"availability":[{"device":"press","topic":"plant/a/status","online":"on","offline":"off"}]}`,
			devices: []string{"press"},
			check: func(t *testing.T, c Connection) {
				want := Connection{
					ClientID:          "edge-1",
					Auth:              &Auth{User: "u", Password: "secret://keystore/mqtt"},
					TLS:               &TLS{CAFile: "/ca.pem", CertFile: "/c.pem", KeyFile: "/k.pem", ServerName: "b.example"},
					Format:            "json",
					QoS:               0,
					PersistentSession: false,
					KeepAlive:         10 * time.Second,
					MaxPayload:        1 << 20,
					Timestamp:         &TimestampSpec{Path: "meta.ts", Format: "unix_ms"},
					Topics:            map[string]TopicOverride{"plant/a": {Timestamp: &TimestampSpec{Path: "t", Format: "rfc3339"}}},
					Availability:      map[string]Availability{"press": {Topic: "plant/a/status", Online: "on", Offline: "off"}},
				}
				if c.Broker == nil || c.Broker.String() != "mqtts://b:8884" {
					t.Errorf("broker = %v", c.Broker)
				}
				c.Broker = nil
				if !reflect.DeepEqual(c, want) {
					t.Errorf("got %+v\nwant %+v", c, want)
				}
			},
		},
		{
			name: "auth anonymous",
			raw:  `{"broker":"mqtt://b","auth":"anonymous"}`,
			check: func(t *testing.T, c Connection) {
				if c.Auth != nil {
					t.Errorf("auth = %+v", c.Auth)
				}
			},
		},
		{
			name: "tls with ca only",
			raw:  `{"broker":"mqtts://b","tls":{"ca_file":"/ca.pem"}}`,
			check: func(t *testing.T, c Connection) {
				if c.TLS == nil || c.TLS.CAFile != "/ca.pem" || c.TLS.CertFile != "" {
					t.Errorf("tls = %+v", c.TLS)
				}
			},
		},
		{
			name: "max_payload one byte",
			raw:  `{"broker":"mqtt://b","max_payload":1}`,
			check: func(t *testing.T, c Connection) {
				if c.MaxPayload != 1 {
					t.Errorf("max_payload = %d", c.MaxPayload)
				}
			},
		},
		{
			name: "max_payload upper bound",
			raw:  `{"broker":"mqtt://b","max_payload":"16MiB"}`,
			check: func(t *testing.T, c Connection) {
				if c.MaxPayload != 16<<20 {
					t.Errorf("max_payload = %d", c.MaxPayload)
				}
			},
		},
		{
			name: "max_payload KiB",
			raw:  `{"broker":"mqtt://b","max_payload":"64KiB"}`,
			check: func(t *testing.T, c Connection) {
				if c.MaxPayload != 64<<10 {
					t.Errorf("max_payload = %d", c.MaxPayload)
				}
			},
		},
		{
			name: "timestamp unix_s",
			raw:  `{"broker":"mqtt://b","timestamp":{"path":"ts","format":"unix_s"}}`,
			check: func(t *testing.T, c Connection) {
				if c.Timestamp == nil || c.Timestamp.Format != "unix_s" {
					t.Errorf("timestamp = %+v", c.Timestamp)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseConnection(json.RawMessage(tt.raw), testInput(tt.devices...))
			if err != nil {
				t.Fatalf("ParseConnection: %v", err)
			}
			tt.check(t, c)
		})
	}
}

const (
	wantBrokerShape = "connection.broker: want mqtt://<host>[:<port>] or mqtts://<host>[:<port>]"
	payloadWant     = "connection.max_payload: want 1 byte to 16MiB, as bytes or a size such as 256KiB"
	tsFormat        = "want rfc3339, unix_s or unix_ms"
)

func TestParseConnectionRefusals(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		devices []string
		want    string
	}{
		{"not an object", `[]`, nil, "connection: must be an object"},
		{"null", `null`, nil, "connection: must be an object"},
		{"empty", ``, nil, "connection: must be an object"},
		{"unknown field", `{"broker":"mqtt://b","bogus":1}`, nil, "connection.bogus: unknown field"},
		{"broker missing", `{}`, nil, "connection.broker: required"},
		{"broker not a string", `{"broker":1}`, nil, "connection.broker: must be a string"},
		{"broker tcp scheme", `{"broker":"tcp://b"}`, nil, wantBrokerShape},
		{"broker ws scheme", `{"broker":"ws://b"}`, nil, wantBrokerShape},
		{"broker no host", `{"broker":"mqtt://"}`, nil, wantBrokerShape},
		{"broker bare host", `{"broker":"broker.local"}`, nil, wantBrokerShape},
		{"broker path", `{"broker":"mqtt://b/path"}`, nil, "connection.broker: must not contain a path or query"},
		{"broker trailing slash", `{"broker":"mqtt://b/"}`, nil, "connection.broker: must not contain a path or query"},
		{"broker query", `{"broker":"mqtt://b?x=1"}`, nil, "connection.broker: must not contain a path or query"},
		{"broker fragment", `{"broker":"mqtt://b#x"}`, nil, "connection.broker: must not contain a path or query"},
		{"broker credentials", `{"broker":"mqtt://u:p@b"}`, nil, "connection.broker: must not contain credentials"},
		{"broker port zero", `{"broker":"mqtt://b:0"}`, nil, wantBrokerShape},
		{"broker port too large", `{"broker":"mqtt://b:70000"}`, nil, wantBrokerShape},
		{"tls on mqtt", `{"broker":"mqtt://b","tls":{"ca_file":"/ca.pem"}}`, nil, "connection.tls: needs an mqtts:// broker"},
		{"tls not an object", `{"broker":"mqtts://b","tls":"yes"}`, nil, "connection.tls: must be an object"},
		{"tls unknown field", `{"broker":"mqtts://b","tls":{"insecure":true}}`, nil, "connection.tls.insecure: unknown field"},
		{"tls cert without key", `{"broker":"mqtts://b","tls":{"cert_file":"/c.pem"}}`, nil, "connection.tls.cert_file: needs key_file"},
		{"tls key without cert", `{"broker":"mqtts://b","tls":{"key_file":"/k.pem"}}`, nil, "connection.tls.key_file: needs cert_file"},
		{"tls ca not a string", `{"broker":"mqtts://b","tls":{"ca_file":1}}`, nil, "connection.tls.ca_file: must be a string"},
		{"auth wrong shape", `{"broker":"mqtt://b","auth":1}`, nil, `connection.auth: want "anonymous" or {user, password}`},
		{"auth other string", `{"broker":"mqtt://b","auth":"basic"}`, nil, `connection.auth: want "anonymous" or {user, password}`},
		{"auth unknown field", `{"broker":"mqtt://b","auth":{"user":"u","password":"secret://k","token":"x"}}`, nil, "connection.auth.token: unknown field"},
		{"auth user missing", `{"broker":"mqtt://b","auth":{"password":"secret://k"}}`, nil, "connection.auth.user: required"},
		{"auth password missing", `{"broker":"mqtt://b","auth":{"user":"u"}}`, nil, "connection.auth.password: required"},
		{"auth password literal", `{"broker":"mqtt://b","auth":{"user":"u","password":"hunter2"}}`, nil, "connection.auth.password: must be a secret:// reference"},
		{"auth password bare scheme", `{"broker":"mqtt://b","auth":{"user":"u","password":"secret://"}}`, nil, "connection.auth.password: must be a secret:// reference"},
		{"client_id not a string", `{"broker":"mqtt://b","client_id":3}`, nil, "connection.client_id: must be a string"},
		{"client_id empty", `{"broker":"mqtt://b","client_id":""}`, nil, "connection.client_id: must not be empty"},
		{"format sparkplug_b", `{"broker":"mqtt://b","format":"sparkplug_b"}`, nil, "connection.format: not available in this build"},
		{"format other", `{"broker":"mqtt://b","format":"xml"}`, nil, "connection.format: want json or sparkplug_b"},
		{"format not a string", `{"broker":"mqtt://b","format":1}`, nil, "connection.format: must be a string"},
		{"rebirth true", `{"broker":"mqtt://b","rebirth":true}`, nil, "connection.rebirth: not available in this build"},
		{"rebirth false", `{"broker":"mqtt://b","rebirth":false}`, nil, "connection.rebirth: not available in this build"},
		{"qos 2", `{"broker":"mqtt://b","qos":2}`, nil, "connection.qos: want 0 or 1"},
		{"qos negative", `{"broker":"mqtt://b","qos":-1}`, nil, "connection.qos: want 0 or 1"},
		{"qos fraction", `{"broker":"mqtt://b","qos":0.5}`, nil, "connection.qos: want 0 or 1"},
		{"qos string", `{"broker":"mqtt://b","qos":"1"}`, nil, "connection.qos: want 0 or 1"},
		{"persistent_session string", `{"broker":"mqtt://b","persistent_session":"yes"}`, nil, "connection.persistent_session: must be true or false"},
		{"keepalive zero", `{"broker":"mqtt://b","keepalive":"0s"}`, nil, "connection.keepalive: want a positive duration such as 30s"},
		{"keepalive negative", `{"broker":"mqtt://b","keepalive":"-5s"}`, nil, "connection.keepalive: want a positive duration such as 30s"},
		{"keepalive number", `{"broker":"mqtt://b","keepalive":30}`, nil, "connection.keepalive: want a positive duration such as 30s"},
		{"keepalive garbage", `{"broker":"mqtt://b","keepalive":"soon"}`, nil, "connection.keepalive: want a positive duration such as 30s"},
		{"max_payload zero", `{"broker":"mqtt://b","max_payload":0}`, nil, payloadWant},
		{"max_payload above limit", `{"broker":"mqtt://b","max_payload":16777217}`, nil, payloadWant},
		{"max_payload 17MiB", `{"broker":"mqtt://b","max_payload":"17MiB"}`, nil, payloadWant},
		{"max_payload KB", `{"broker":"mqtt://b","max_payload":"256KB"}`, nil, payloadWant},
		{"max_payload zero KiB", `{"broker":"mqtt://b","max_payload":"0KiB"}`, nil, payloadWant},
		{"max_payload fraction", `{"broker":"mqtt://b","max_payload":1.5}`, nil, payloadWant},
		{"max_payload negative", `{"broker":"mqtt://b","max_payload":"-1KiB"}`, nil, payloadWant},
		{"max_payload bare string", `{"broker":"mqtt://b","max_payload":"1024"}`, nil, payloadWant},
		{"max_payload overflow", `{"broker":"mqtt://b","max_payload":"99999999999999999999KiB"}`, nil, payloadWant},
		{"timestamp not an object", `{"broker":"mqtt://b","timestamp":"ts"}`, nil, "connection.timestamp: must be an object"},
		{"timestamp unknown field", `{"broker":"mqtt://b","timestamp":{"path":"t","format":"unix_s","zone":"x"}}`, nil, "connection.timestamp.zone: unknown field"},
		{"timestamp path missing", `{"broker":"mqtt://b","timestamp":{"format":"unix_s"}}`, nil, "connection.timestamp.path: required"},
		{"timestamp path empty segment", `{"broker":"mqtt://b","timestamp":{"path":"a..b","format":"unix_s"}}`, nil, "connection.timestamp.path: must not contain an empty segment"},
		{"timestamp format missing", `{"broker":"mqtt://b","timestamp":{"path":"t"}}`, nil, "connection.timestamp.format: required"},
		{"timestamp format other", `{"broker":"mqtt://b","timestamp":{"path":"t","format":"iso"}}`, nil, "connection.timestamp.format: " + tsFormat},
		{"topics not an object", `{"broker":"mqtt://b","topics":[]}`, nil, "connection.topics: must be an object"},
		{"topics wildcard", `{"broker":"mqtt://b","topics":{"a/+":{}}}`, nil, "connection.topics: topic must be non-empty without + or #"},
		{"topics empty key", `{"broker":"mqtt://b","topics":{"":{}}}`, nil, "connection.topics: topic must be non-empty without + or #"},
		{"topics override not an object", `{"broker":"mqtt://b","topics":{"a":1}}`, nil, "connection.topics.*: must be an object"},
		{"topics override unknown field", `{"broker":"mqtt://b","topics":{"a":{"qos":0}}}`, nil, "connection.topics.*.qos: unknown field"},
		{"topics override timestamp format", `{"broker":"mqtt://b","topics":{"a":{"timestamp":{"path":"t","format":"x"}}}}`, nil, "connection.topics.*.timestamp.format: " + tsFormat},
		{"availability not a list", `{"broker":"mqtt://b","availability":{}}`, nil, "connection.availability: must be a list"},
		{"availability entry not an object", `{"broker":"mqtt://b","availability":[1]}`, []string{"d"}, "connection.availability[0]: must be an object"},
		{"availability unknown field", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"1","offline":"0","x":1}]}`, []string{"d"}, "connection.availability[0].x: unknown field"},
		{"availability device missing", `{"broker":"mqtt://b","availability":[{"topic":"t","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[0].device: required"},
		{"availability unknown device", `{"broker":"mqtt://b","availability":[{"device":"ghost","topic":"t","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[0].device: not a device of this input"},
		{"availability no devices", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"1","offline":"0"}]}`, nil, "connection.availability[0].device: not a device of this input"},
		{"availability duplicate device", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"1","offline":"0"},{"device":"d","topic":"u","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[1].device: already has an availability entry"},
		{"availability topic missing", `{"broker":"mqtt://b","availability":[{"device":"d","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[0].topic: required"},
		{"availability topic wildcard", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"a/#","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[0].topic: must not contain + or #"},
		{"availability online empty", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"","offline":"0"}]}`, []string{"d"}, "connection.availability[0].online: required"},
		{"availability offline missing", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"1"}]}`, []string{"d"}, "connection.availability[0].offline: required"},
		{"topics key no point uses", `{"broker":"mqtt://b","topics":{"plant/a":{},"plant/b":{}}}`, []string{"d"}, "connection.topics: a key is not the topic of a point"},
		{"availability topic of a point", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"plant/a","online":"1","offline":"0"}]}`, []string{"d"}, "connection.availability[0].topic: also the topic of a point"},
		{"availability online equals offline", `{"broker":"mqtt://b","availability":[{"device":"d","topic":"t","online":"x","offline":"x"}]}`, []string{"d"}, "connection.availability[0].offline: must differ from online"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConnection(json.RawMessage(tt.raw), testInput(tt.devices...))
			if err == nil {
				t.Fatal("want an error")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParseConnectionErrorsNeverEchoValues(t *testing.T) {
	const secret = "hunter2-sekret"
	raws := []string{
		`{"broker":"mqtt://u:` + secret + `@b"}`,
		`{"broker":"` + secret + `://b"}`,
		`{"broker":"mqtt://b/` + secret + `"}`,
		`{"broker":"mqtt://b","auth":{"user":"u","password":"` + secret + `"}}`,
		`{"broker":"mqtt://b","auth":"` + secret + `"}`,
		`{"broker":"mqtt://b","` + secret + `":1}`,
		`{"broker":"mqtt://b","format":"` + secret + `"}`,
		`{"broker":"mqtt://b","keepalive":"` + secret + `"}`,
		`{"broker":"mqtt://b","max_payload":"` + secret + `"}`,
		`{"broker":"mqtt://b","timestamp":{"path":"t","format":"` + secret + `"}}`,
		`{"broker":"mqtt://b","topics":{"` + secret + `/+":{}}}`,
		`{"broker":"mqtt://b","topics":{"` + secret + `":{"` + secret + `":1}}}`,
		`{"broker":"mqtts://b","tls":{"` + secret + `":1}}`,
		`{"broker":"mqtts://b","tls":{"cert_file":"/` + secret + `"}}`,
		`{"broker":"mqtt://b","availability":[{"device":"` + secret + `","topic":"t","online":"1","offline":"0"}]}`,
		`{"broker":"mqtt://b","availability":[{"device":"d","topic":"` + secret + `/#","online":"1","offline":"0"}]}`,
	}
	for _, raw := range raws {
		_, err := ParseConnection(json.RawMessage(raw), testInput("d"))
		if err == nil {
			t.Errorf("%s: want an error", raw)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error echoes a value: %q", raw, err)
		}
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in   string
		want Address
	}{
		{"a/b", Address{Topic: "a/b"}},
		{"a/b|x.y.0", Address{Topic: "a/b", Path: []string{"x", "y", "0"}}},
		{"a/b|x|y", Address{Topic: "a/b", Path: []string{"x|y"}}},
		{"a/b|temp", Address{Topic: "a/b", Path: []string{"temp"}}},
		{"/leading/slash", Address{Topic: "/leading/slash"}},
		{"$SYS/broker/load", Address{Topic: "$SYS/broker/load"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAddress(tt.in)
			if err != nil {
				t.Fatalf("ParseAddress: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseAddressRefusals(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "address: required"},
		{"empty topic with path", "|x", "address: topic: required"},
		{"plus", "a/+", "address: topic: must not contain + or #"},
		{"plus with path", "a/+/b|x", "address: topic: must not contain + or #"},
		{"hash", "a/#", "address: topic: must not contain + or #"},
		{"empty path", "a|", "address: path: must not be empty after |"},
		{"empty segment middle", "a|x..y", "address: path: must not contain an empty segment"},
		{"empty segment leading", "a|.x", "address: path: must not contain an empty segment"},
		{"empty segment trailing", "a|x.", "address: path: must not contain an empty segment"},
		{"dot only", "a|.", "address: path: must not contain an empty segment"},
		{"nul in topic", "a\x00b", "address: topic: must not contain a NUL character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAddress(tt.in)
			if err == nil {
				t.Fatal("want an error")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestCheckPoint(t *testing.T) {
	sub := config.Read{Mode: config.ReadSubscribe}
	long := config.Read{Mode: config.ReadSubscribe, Subscribe: &config.Subscribe{Sampling: time.Second, Queue: 4, Deadband: "1"}}
	tests := []struct {
		name string
		p    config.Point
		want string
	}{
		{"short form", config.Point{Read: sub, Type: "float64"}, ""},
		{"long form accepted", config.Point{Read: long, Type: "int32"}, ""},
		{"poll", config.Point{Read: config.Read{Mode: config.ReadPoll, Interval: time.Second}, Type: "int32"}, "read: want subscribe"},
		{"event", config.Point{Read: config.Read{Mode: config.ReadEvent}, Type: "int32"}, "read: want subscribe"},
		{"no mode", config.Point{Type: "int32"}, "read: want subscribe"},
		{"type missing", config.Point{Read: sub}, "type: required"},
		{"type unknown", config.Point{Read: sub, Type: "decimal"}, "type: want bool, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string"},
		{"byte order", config.Point{Read: sub, Type: "int32", ByteOrder: "ABCD"}, "byte_order: not for mqtt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPoint(tt.p)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("CheckPoint: %v", err)
			case tt.want != "" && err == nil:
				t.Fatal("want an error")
			case tt.want != "" && err.Error() != tt.want:
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestCheckPointAcceptsEveryType(t *testing.T) {
	for _, typ := range []string{"bool", "int16", "uint16", "int32", "uint32", "int64", "uint64", "float32", "float64", "string"} {
		if err := CheckPoint(config.Point{Read: config.Read{Mode: config.ReadSubscribe}, Type: typ}); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}
