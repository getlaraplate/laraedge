// Package logging builds the agent logger: structured JSON lines, one per
// record, at the level the operator chose (spec 10.5). At info and above the
// agent logs no secret and no sample value; that is a rule of every log
// call, verified by a scan of the logs of a full run. As a second line of
// defence the handler redacts any attribute whose key names a secret.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Redacted replaces the value of an attribute whose key names a secret.
const Redacted = "[redacted]"

// secretKeys are the key names, or key suffixes, of secret attributes: the
// same rule the configuration applies to secret fields (ruling R44).
var secretKeys = []string{"token", "password", "secret", "api_key"}

// ParseLevel returns the level named by s: debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: use debug, info, warn or error", s)
	}
}

// New returns a logger writing JSON lines to w from level up. An attribute
// whose key is, or ends with, token, password, secret or api_key (in any
// case) is written as Redacted.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	}))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup || !isSecretKey(a.Key) {
		return a
	}

	return slog.String(a.Key, Redacted)
}

func isSecretKey(key string) bool {
	key = strings.ToLower(key)
	for _, s := range secretKeys {
		if strings.HasSuffix(key, s) {
			return true
		}
	}

	return false
}
