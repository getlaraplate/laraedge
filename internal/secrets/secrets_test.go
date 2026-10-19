package secrets

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sentinel = "s3cr3t-sentinel-value"

func fakeEnv(vars map[string]string) Option {
	return WithLookupEnv(func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	})
}

func fakeFile(path *string, content string, mode fs.FileMode, err error) Option {
	return WithReadFile(func(p string) ([]byte, fs.FileMode, error) {
		*path = p
		return []byte(content), mode, err
	})
}

func TestIsReference(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"secret://env/TOKEN", true},
		{"secret://file/etc/token", true},
		{"secret://keystore/TOKEN", true},
		{"secret://", true},
		{"plain-token-value", false},
		{"", false},
		{"Secret://env/TOKEN", false},
		{" secret://env/TOKEN", false},
	}
	for _, tt := range tests {
		if got := IsReference(tt.in); got != tt.want {
			t.Errorf("IsReference(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		wantErr     string
		unavailable bool
	}{
		{name: "env", ref: "secret://env/LARAPLATE_TOKEN"},
		{name: "env underscore start", ref: "secret://env/_X1"},
		{name: "file", ref: "secret://file/etc/laraplate-edge/token"},
		{name: "keystore is unavailable", ref: "secret://keystore/token", wantErr: "not available in this build", unavailable: true},
		{name: "not a reference", ref: "plain-" + sentinel, wantErr: "not a secret:// reference"},
		{name: "unknown kind", ref: "secret://vault/token", wantErr: "unknown secret kind"},
		{name: "no kind", ref: "secret://", wantErr: "unknown secret kind"},
		{name: "env bad name digit first", ref: "secret://env/1ABC", wantErr: "invalid environment variable name"},
		{name: "env bad name dash", ref: "secret://env/A-B", wantErr: "invalid environment variable name"},
		{name: "env empty name", ref: "secret://env/", wantErr: "invalid environment variable name"},
		{name: "env no slash", ref: "secret://env", wantErr: "invalid environment variable name"},
		{name: "file empty path", ref: "secret://file/", wantErr: "empty file path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.ref)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Check(%q) = %v, want nil", tt.ref, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check(%q) = nil, want error containing %q", tt.ref, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if strings.Contains(tt.ref, sentinel) && strings.Contains(err.Error(), sentinel) {
				t.Errorf("error echoes the input: %q", err)
			}
			if errors.Is(err, ErrUnavailable) != tt.unavailable {
				t.Errorf("errors.Is(err, ErrUnavailable) = %v, want %v", !tt.unavailable, tt.unavailable)
			}
		})
	}
}

func TestResolveEnv(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		ref     string
		want    string
		wantErr string
	}{
		{name: "set", vars: map[string]string{"TOKEN": "abc"}, ref: "secret://env/TOKEN", want: "abc"},
		{name: "value kept verbatim", vars: map[string]string{"TOKEN": " a b\n"}, ref: "secret://env/TOKEN", want: " a b\n"},
		{name: "unset", vars: map[string]string{"OTHER": sentinel}, ref: "secret://env/TOKEN", wantErr: "secret://env/TOKEN"},
		{name: "empty", vars: map[string]string{"TOKEN": ""}, ref: "secret://env/TOKEN", wantErr: "secret://env/TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(fakeEnv(tt.vars))
			got, err := r.Resolve(context.Background(), tt.ref)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Resolve = %q, want error", got)
				}
				if got != "" {
					t.Errorf("value returned with error: %q", got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not name the reference %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), sentinel) {
					t.Errorf("error leaks a value: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveFileContentAndPathMapping(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		ref      string
		content  string
		mode     fs.FileMode
		wantPath string
		want     string
	}{
		{name: "unix absolute path", goos: "linux", ref: "secret://file/etc/laraplate-edge/token", content: "tok", mode: 0o600, wantPath: "/etc/laraplate-edge/token", want: "tok"},
		{name: "trims one LF", goos: "linux", ref: "secret://file/t", content: "tok\n", mode: 0o600, wantPath: "/t", want: "tok"},
		{name: "trims one CRLF", goos: "linux", ref: "secret://file/t", content: "tok\r\n", mode: 0o400, wantPath: "/t", want: "tok"},
		{name: "trims only one newline", goos: "linux", ref: "secret://file/t", content: "tok\n\n", mode: 0o600, wantPath: "/t", want: "tok\n"},
		{name: "keeps inner spaces", goos: "linux", ref: "secret://file/t", content: " a b \n", mode: 0o600, wantPath: "/t", want: " a b "},
		{name: "darwin behaves like unix", goos: "darwin", ref: "secret://file/t", content: "tok", mode: 0o600, wantPath: "/t", want: "tok"},
		{name: "windows verbatim path", goos: "windows", ref: "secret://file/C:/ProgramData/laraplate-edge/token", content: "tok", mode: 0o666, wantPath: "C:/ProgramData/laraplate-edge/token", want: "tok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			r := NewResolver(WithGOOS(tt.goos), fakeFile(&gotPath, tt.content, tt.mode, nil))
			got, err := r.Resolve(context.Background(), tt.ref)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Errorf("Resolve = %q, want %q", got, tt.want)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}

func TestResolveFileRefusals(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		content string
		mode    fs.FileMode
		readErr error
		wantErr string
	}{
		{name: "group readable", goos: "linux", content: sentinel, mode: 0o640, wantErr: "0640"},
		{name: "world readable", goos: "linux", content: sentinel, mode: 0o604, wantErr: "0604"},
		{name: "0644", goos: "linux", content: sentinel, mode: 0o644, wantErr: "0644"},
		{name: "group writable", goos: "freebsd", content: sentinel, mode: 0o620, wantErr: "0620"},
		{name: "empty file", goos: "linux", content: "\n", mode: 0o600, wantErr: "empty"},
		{name: "read error", goos: "linux", readErr: fs.ErrNotExist, wantErr: "secret://file/t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p string
			r := NewResolver(WithGOOS(tt.goos), fakeFile(&p, tt.content, tt.mode, tt.readErr))
			got, err := r.Resolve(context.Background(), "secret://file/t")
			if err == nil {
				t.Fatalf("Resolve = %q, want error", got)
			}
			if got != "" {
				t.Errorf("value returned with error: %q", got)
			}
			if !strings.Contains(err.Error(), "secret://file/t") {
				t.Errorf("error %q does not name the reference", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error leaks a value: %q", err)
			}
			if tt.readErr != nil && !errors.Is(err, tt.readErr) {
				t.Errorf("error does not wrap the read error: %v", err)
			}
		})
	}
}

func TestResolveRealFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	ref := "secret://file/" + strings.TrimPrefix(filepath.ToSlash(path), "/")

	if err := os.WriteFile(path, []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver()
	got, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve 0600: %v", err)
	}
	if got != sentinel {
		t.Errorf("Resolve 0600 = %q, want %q", got, sentinel)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = r.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatalf("Resolve 0644 = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "0644") || !strings.Contains(err.Error(), ref) {
		t.Errorf("error %q must name the mode and the reference", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error leaks a value: %q", err)
	}

	missing := "secret://file/" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(dir, "absent")), "/")
	if _, err := r.Resolve(context.Background(), missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing file error %v must name the reference", err)
	}
}

func TestResolveKeystoreIsUnavailable(t *testing.T) {
	r := NewResolver()
	_, err := r.Resolve(context.Background(), "secret://keystore/laraplate")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("errors.Is(err, ErrUnavailable) = false for %v", err)
	}
	for _, want := range []string{"secret://keystore/laraplate", "not available in this build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestResolveInvalidReferences(t *testing.T) {
	r := NewResolver()
	for _, ref := range []string{"plain-" + sentinel, "secret://vault/x", "secret://env/9X", "secret://file/"} {
		got, err := r.Resolve(context.Background(), ref)
		if err == nil {
			t.Errorf("Resolve(%q) = %q, want error", ref, got)
			continue
		}
		if strings.Contains(ref, sentinel) && strings.Contains(err.Error(), sentinel) {
			t.Errorf("error echoes a non-reference input: %q", err)
		}
	}
}

func TestResolveHonoursCancelledContext(t *testing.T) {
	called := false
	r := NewResolver(WithLookupEnv(func(string) (string, bool) { called = true; return "v", true }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(ctx, "secret://env/TOKEN"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if called {
		t.Error("env was consulted despite a cancelled context")
	}
}

func TestResolveRealFileSizeLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a Unix path mapping")
	}
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at the limit", size: maxSecretSize},
		{name: "one byte over", size: maxSecretSize + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(strings.Repeat("a", tt.size)), 0o600); err != nil {
				t.Fatal(err)
			}
			ref := "secret://file/" + strings.TrimPrefix(filepath.ToSlash(path), "/")
			got, err := NewResolver().Resolve(context.Background(), ref)
			if !tt.wantErr {
				if err != nil || len(got) != tt.size {
					t.Fatalf("Resolve = %d bytes, %v; want %d bytes", len(got), err, tt.size)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve returned %d bytes, want an error", len(got))
			}
			if !strings.Contains(err.Error(), ref) || !strings.Contains(err.Error(), "larger than") {
				t.Errorf("error %q must name the reference and the limit", err)
			}
		})
	}
}
