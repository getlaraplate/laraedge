package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const hashManifest = `{"kinds":{"output":{"config_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}}},"name":"demo","protocol_versions":["v1"],"version":"0.1.0"}`

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func hashBinaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}

	return name
}

func writeHashFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginHash(t *testing.T) {
	binary := "not really a binary"

	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		args       func(dir string) []string
		wantCode   int
		wantStdout func(dir string) string
		wantStderr []string
	}{
		{
			name: "prints the hashes of the binary and the manifest",
			setup: func(t *testing.T, dir string) {
				writeHashFile(t, filepath.Join(dir, "plugin.json"), hashManifest)
				writeHashFile(t, filepath.Join(dir, hashBinaryName("demo")), binary)
			},
			args:     func(dir string) []string { return []string{"plugin", "hash", dir} },
			wantCode: 0,
			wantStdout: func(string) string {
				return "sha256: " + hashHex([]byte(binary)) + "\nmanifest_sha256: " + hashHex([]byte(hashManifest)) + "\n"
			},
		},
		{
			name:       "missing manifest",
			setup:      func(*testing.T, string) {},
			args:       func(dir string) []string { return []string{"plugin", "hash", dir} },
			wantCode:   1,
			wantStderr: []string{"laraplate-edge plugin hash:", "plugin.json"},
		},
		{
			name: "missing binary names the file",
			setup: func(t *testing.T, dir string) {
				writeHashFile(t, filepath.Join(dir, "plugin.json"), hashManifest)
			},
			args:       func(dir string) []string { return []string{"plugin", "hash", dir} },
			wantCode:   1,
			wantStderr: []string{"laraplate-edge plugin hash:", hashBinaryName("demo")},
		},
		{
			name: "invalid manifest",
			setup: func(t *testing.T, dir string) {
				writeHashFile(t, filepath.Join(dir, "plugin.json"), `{"name":`)
			},
			args:       func(dir string) []string { return []string{"plugin", "hash", dir} },
			wantCode:   1,
			wantStderr: []string{"laraplate-edge plugin hash:"},
		},
		{
			name: "manifest name with a path separator",
			setup: func(t *testing.T, dir string) {
				writeHashFile(t, filepath.Join(dir, "plugin.json"), strings.Replace(hashManifest, `"name":"demo"`, `"name":"../demo"`, 1))
			},
			args:       func(dir string) []string { return []string{"plugin", "hash", dir} },
			wantCode:   1,
			wantStderr: []string{"plugin.json: name must not contain a path separator or .."},
		},
		{
			name:       "nonexistent directory",
			setup:      func(*testing.T, string) {},
			args:       func(dir string) []string { return []string{"plugin", "hash", filepath.Join(dir, "absent")} },
			wantCode:   1,
			wantStderr: []string{"laraplate-edge plugin hash:", "plugin.json"},
		},
		{
			name:       "no directory",
			setup:      func(*testing.T, string) {},
			args:       func(string) []string { return []string{"plugin", "hash"} },
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
		{
			name:       "unknown plugin subcommand",
			setup:      func(*testing.T, string) {},
			args:       func(dir string) []string { return []string{"plugin", "frob", dir} },
			wantCode:   2,
			wantStderr: []string{"usage:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			var stdout, stderr bytes.Buffer
			code := run(tt.args(dir), &stdout, &stderr)

			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != nil && stdout.String() != tt.wantStdout(dir) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout(dir))
			}
			if tt.wantStdout == nil && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestUsageListsPluginHash(t *testing.T) {
	if !strings.Contains(usageText, "plugin hash <dir>") {
		t.Error("usage text does not list plugin hash")
	}
}
