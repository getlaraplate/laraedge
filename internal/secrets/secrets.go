// Package secrets resolves secret:// references. The agent configuration
// holds only references; the values are read here, at the moment of use, and
// never appear in an error message or a log line.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strings"
)

const (
	prefix = "secret://"

	// maxSecretSize bounds how much of a secret file is read.
	maxSecretSize = 1 << 20
)

// ErrUnavailable is returned for references the build cannot resolve, such as
// secret://keystore/... (the OS keystore is not part of this build).
var ErrUnavailable = errors.New("not available in this build")

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsReference reports whether s is a secret:// reference, whether or not it is
// well formed. Use Check to validate the syntax.
func IsReference(s string) bool {
	return strings.HasPrefix(s, prefix)
}

type kind string

const (
	kindEnv      kind = "env"
	kindFile     kind = "file"
	kindKeystore kind = "keystore"
)

// parse splits a reference into its kind and the part after the kind. The
// errors it returns for malformed references never echo the reference: a
// secret written where a reference belongs must not reach a log. Well formed
// references (env with a valid name, file, keystore) may be named by callers.
func parse(ref string) (kind, string, error) {
	if !IsReference(ref) {
		return "", "", errors.New("value is not a secret:// reference")
	}
	k, rest, _ := strings.Cut(strings.TrimPrefix(ref, prefix), "/")
	switch kind(k) {
	case kindEnv:
		if !envName.MatchString(rest) {
			return "", "", errors.New("secret://env/...: invalid environment variable name")
		}
	case kindFile:
		if rest == "" {
			return "", "", errors.New("secret://file/...: empty file path")
		}
	case kindKeystore:
		return kindKeystore, rest, fmt.Errorf("secret %s: %w", ref, ErrUnavailable)
	default:
		return "", "", errors.New("unknown secret kind (want secret://env/, secret://file/ or secret://keystore/)")
	}
	return kind(k), rest, nil
}

// Check validates the syntax of ref without resolving it. It returns an error
// wrapping ErrUnavailable for keystore references.
func Check(ref string) error {
	_, _, err := parse(ref)
	return err
}

// Option customises a Resolver, mainly so tests can inject fakes.
type Option func(*Resolver)

// WithLookupEnv replaces os.LookupEnv.
func WithLookupEnv(f func(name string) (string, bool)) Option {
	return func(r *Resolver) { r.lookupEnv = f }
}

// WithReadFile replaces the file reader. The function returns the content and
// the permission bits of the file it read.
func WithReadFile(f func(path string) ([]byte, fs.FileMode, error)) Option {
	return func(r *Resolver) { r.readFile = f }
}

// WithGOOS overrides the operating system used to map file references to paths
// and to decide on the permission check.
func WithGOOS(goos string) Option {
	return func(r *Resolver) { r.goos = goos }
}

// Resolver turns secret:// references into values.
type Resolver struct {
	lookupEnv func(string) (string, bool)
	readFile  func(string) ([]byte, fs.FileMode, error)
	goos      string
}

// NewResolver returns a Resolver reading the real environment and file system.
func NewResolver(opts ...Option) *Resolver {
	r := &Resolver{lookupEnv: os.LookupEnv, readFile: readSecretFile, goos: runtime.GOOS}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve returns the value ref points to. Every error names ref and none
// contains a value.
//
//	secret://env/NAME        the environment variable NAME, which must be set and non-empty
//	secret://file/<path>     the file content with one trailing newline trimmed; on Unix the
//	                         path is "/"+<path> and the file must not be group or world accessible
//	secret://keystore/NAME   always ErrUnavailable in this build
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	k, rest, err := parse(ref)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("secret %s: %w", ref, err)
	}
	switch k {
	case kindEnv:
		v, ok := r.lookupEnv(rest)
		if !ok || v == "" {
			return "", fmt.Errorf("secret %s: environment variable %s is not set or empty", ref, rest)
		}
		return v, nil
	default:
		return r.resolveFile(ref, rest)
	}
}

func (r *Resolver) resolveFile(ref, rest string) (string, error) {
	windows := r.goos == "windows"
	path := rest
	if !windows {
		path = "/" + rest
	}
	data, mode, err := r.readFile(path)
	if err != nil {
		return "", fmt.Errorf("secret %s: read file: %w", ref, err)
	}
	if !windows && mode.Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret %s: file mode %04o is readable or writable by group or others, want 0600 or stricter", ref, mode.Perm())
	}
	value := string(data)
	if v, ok := strings.CutSuffix(value, "\r\n"); ok {
		value = v
	} else if v, ok := strings.CutSuffix(value, "\n"); ok {
		value = v
	}
	if value == "" {
		return "", fmt.Errorf("secret %s: file is empty", ref)
	}
	return value, nil
}

// readSecretFile opens path once and takes the mode from the open descriptor,
// so the checked file is the one read.
func readSecretFile(path string) ([]byte, fs.FileMode, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxSecretSize {
		return nil, 0, fmt.Errorf("file is larger than %d bytes", maxSecretSize)
	}
	return data, info.Mode(), nil
}
