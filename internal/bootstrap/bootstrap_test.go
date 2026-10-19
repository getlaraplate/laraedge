package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	plainToken = "plain-token-value-1234"
	shortToken = "tok-s3cr3t"
)

func hostDataDir() string { return DefaultDataDir(runtime.GOOS, os.Getenv) }

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bootstrap.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want File
	}{
		{
			name: "file provider with defaults",
			yaml: "agent_id: edge-1\nconfig:\n  provider: file\n  file: /etc/laraplate-edge/config.yaml\n",
			want: File{
				AgentID: "edge-1",
				Config:  Config{Provider: "file", File: "/etc/laraplate-edge/config.yaml", Laraplate: Laraplate{PollSeconds: 60}},
				DataDir: hostDataDir(), StatusListen: "127.0.0.1:9180",
			},
		},
		{
			name: "laraplate provider with defaults",
			yaml: "agent_id: edge-1\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n    token: secret://env/LARAPLATE_TOKEN\n",
			want: File{
				AgentID: "edge-1",
				Config: Config{Provider: "laraplate", Laraplate: Laraplate{
					URL: "https://mes.example.com", Token: "secret://env/LARAPLATE_TOKEN", PollSeconds: 60,
				}},
				DataDir: hostDataDir(), StatusListen: "127.0.0.1:9180",
			},
		},
		{
			name: "everything explicit",
			yaml: "agent_id: edge-2\ndata_dir: /srv/edge\nstatus_listen: 0.0.0.0:9200\nstatus_token: secret://file/etc/edge/status\n" +
				"config:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com/\n    token: secret://file/etc/edge/token\n    poll_seconds: 5\n",
			want: File{
				AgentID: "edge-2", DataDir: "/srv/edge", StatusListen: "0.0.0.0:9200", StatusToken: "secret://file/etc/edge/status",
				Config: Config{Provider: "laraplate", Laraplate: Laraplate{
					URL: "https://mes.example.com/", Token: "secret://file/etc/edge/token", PollSeconds: 5,
				}},
			},
		},
		{
			name: "http allowed on loopback",
			yaml: "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: http://127.0.0.1:8000\n    token: secret://env/T\n",
			want: File{
				AgentID: "a", DataDir: hostDataDir(), StatusListen: "127.0.0.1:9180",
				Config: Config{Provider: "laraplate", Laraplate: Laraplate{URL: "http://127.0.0.1:8000", Token: "secret://env/T", PollSeconds: 60}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(write(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got != tt.want {
				t.Errorf("Load =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestLoadRefusals(t *testing.T) {
	const fileProvider = "config:\n  provider: file\n  file: /c.yaml\n"
	const lara = "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n    token: secret://env/T\n"
	tests := []struct {
		name    string
		yaml    string
		wantErr []string
	}{
		{name: "empty file", yaml: "", wantErr: []string{"agent_id"}},
		{name: "missing agent_id", yaml: fileProvider, wantErr: []string{"agent_id", "required"}},
		{name: "blank agent_id", yaml: "agent_id: \"  \"\n" + fileProvider, wantErr: []string{"agent_id"}},
		{name: "missing provider", yaml: "agent_id: a\n", wantErr: []string{"config.provider", "required"}},
		{name: "unknown provider", yaml: "agent_id: a\nconfig:\n  provider: cloud\n", wantErr: []string{"config.provider", "file", "laraplate"}},
		{name: "file provider without file", yaml: "agent_id: a\nconfig:\n  provider: file\n", wantErr: []string{"config.file", "required"}},
		{name: "laraplate without url", yaml: "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    token: secret://env/T\n", wantErr: []string{"config.laraplate.url", "required"}},
		{name: "laraplate without token", yaml: "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n", wantErr: []string{"config.laraplate.token", "required"}},
		{
			name:    "plain token refused",
			yaml:    "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n    token: " + plainToken + "\n",
			wantErr: []string{"config.laraplate.token", "secret://"},
		},
		{
			name:    "plain status token refused",
			yaml:    "agent_id: a\nstatus_token: " + plainToken + "\n" + fileProvider,
			wantErr: []string{"status_token", "secret://"},
		},
		{
			name:    "keystore token refused",
			yaml:    "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n    token: secret://keystore/t\n",
			wantErr: []string{"config.laraplate.token", "not available in this build"},
		},
		{
			name:    "malformed reference refused",
			yaml:    "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: https://mes.example.com\n    token: secret://vault/t\n",
			wantErr: []string{"config.laraplate.token", "unknown secret kind"},
		},
		{name: "http on non-loopback", yaml: strings.Replace(lara, "https://mes.example.com", "http://mes.example.com", 1), wantErr: []string{"config.laraplate.url", "https"}},
		{name: "relative url", yaml: strings.Replace(lara, "https://mes.example.com", "mes.example.com", 1), wantErr: []string{"config.laraplate.url"}},
		{name: "unsupported scheme", yaml: strings.Replace(lara, "https://mes.example.com", "ftp://mes.example.com", 1), wantErr: []string{"config.laraplate.url"}},
		{name: "url with credentials", yaml: strings.Replace(lara, "https://mes.example.com", "https://user:"+plainToken+"@mes.example.com", 1), wantErr: []string{"config.laraplate.url", "credentials"}},
		{name: "poll below minimum", yaml: lara + "    poll_seconds: 4\n", wantErr: []string{"config.laraplate.poll_seconds", "5"}},
		{name: "poll zero", yaml: lara + "    poll_seconds: 0\n", wantErr: []string{"config.laraplate.poll_seconds"}},
		{name: "poll negative", yaml: lara + "    poll_seconds: -1\n", wantErr: []string{"config.laraplate.poll_seconds"}},
		{name: "status_listen without port", yaml: "agent_id: a\nstatus_listen: localhost\n" + fileProvider, wantErr: []string{"status_listen"}},
		{name: "status_listen bad port", yaml: "agent_id: a\nstatus_listen: 127.0.0.1:http\n" + fileProvider, wantErr: []string{"status_listen"}},
		{name: "status_listen port out of range", yaml: "agent_id: a\nstatus_listen: 127.0.0.1:70000\n" + fileProvider, wantErr: []string{"status_listen"}},
		{name: "status_listen empty", yaml: "agent_id: a\nstatus_listen: \"\"\n" + fileProvider, wantErr: []string{"status_listen"}},
		{name: "data_dir empty", yaml: "agent_id: a\ndata_dir: \"\"\n" + fileProvider, wantErr: []string{"data_dir"}},
		{name: "unknown top-level key", yaml: "agent_id: a\nbogus: 1\n" + fileProvider, wantErr: []string{"bogus"}},
		{name: "unknown nested key", yaml: "agent_id: a\nconfig:\n  provider: file\n  file: /c.yaml\n  extra: 1\n", wantErr: []string{"extra"}},
		{name: "unknown deep key", yaml: lara + "    verify_tls: false\n", wantErr: []string{"verify_tls"}},
		{name: "token as config value", yaml: "agent_id: a\nconfig: " + plainToken + "\n", wantErr: []string{"line 2", "cannot unmarshal !!str into"}},
		{name: "token as laraplate value", yaml: "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate: " + plainToken + "\n", wantErr: []string{"line 4", "cannot unmarshal !!str into"}},
		{name: "short value as poll_seconds", yaml: lara + "    poll_seconds: " + shortToken + "\n", wantErr: []string{"cannot unmarshal !!str into int"}},
		{name: "token as agent_id list", yaml: "agent_id: [" + plainToken + "]\n" + fileProvider, wantErr: []string{"cannot unmarshal !!seq into string"}},
		{name: "second document", yaml: "agent_id: a\n" + fileProvider + "---\nagent_id: b\n", wantErr: []string{"one YAML document"}},
		{name: "not yaml", yaml: "agent_id: [unclosed\n", wantErr: []string{"bootstrap"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(write(t, tt.yaml))
			if err == nil {
				t.Fatalf("Load = %+v, want error", got)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			// yaml.v3 shows up to 7 leading characters of long scalars, so check prefixes too.
			for _, leak := range []string{plainToken, plainToken[:7], shortToken} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error echoes a secret value (%q): %q", leak, err)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("error = %v, want one naming the file", err)
	}
}

func TestLoadAcceptsLoopbackHTTPHosts(t *testing.T) {
	for _, host := range []string{"localhost:8000", "127.0.0.1", "127.1.2.3:80", "[::1]:8000"} {
		t.Run(host, func(t *testing.T) {
			y := "agent_id: a\nconfig:\n  provider: laraplate\n  laraplate:\n    url: http://" + host + "\n    token: secret://env/T\n"
			if _, err := Load(write(t, y)); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestDefaultDataDir(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name   string
		goos   string
		getenv func(string) string
		want   string
	}{
		{name: "linux", goos: "linux", getenv: nil, want: "/var/lib/laraplate-edge"},
		{name: "freebsd", goos: "freebsd", getenv: nil, want: "/var/lib/laraplate-edge"},
		{name: "darwin", goos: "darwin", getenv: nil, want: "/var/lib/laraplate-edge"},
		{name: "windows ProgramData", goos: "windows", getenv: env(map[string]string{"ProgramData": `D:\PD`}), want: `D:\PD\laraplate-edge`},
		{name: "windows trailing separator", goos: "windows", getenv: env(map[string]string{"ProgramData": `D:\PD\`}), want: `D:\PD\laraplate-edge`},
		{name: "windows fallback", goos: "windows", getenv: env(nil), want: `C:\ProgramData\laraplate-edge`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultDataDir(tt.goos, tt.getenv); got != tt.want {
				t.Errorf("DefaultDataDir = %q, want %q", got, tt.want)
			}
		})
	}
}
