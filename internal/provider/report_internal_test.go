package provider

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestReportAgentVersionFitsTheBackend: the backend refuses an agent_version
// above 64 characters with a 422, so a longer build version is cut and an
// invalid UTF-8 one repaired; a report never fails for it.
func TestReportAgentVersionFitsTheBackend(t *testing.T) {
	long := strings.Repeat("v", MaxAgentVersionLength+10)
	accented := strings.Repeat("è", MaxAgentVersionLength+1)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"dev", "dev", "dev"},
		{"at the limit", long[:MaxAgentVersionLength], long[:MaxAgentVersionLength]},
		{"above the limit", long, long[:MaxAgentVersionLength]},
		{"characters, not bytes", accented, strings.Repeat("è", MaxAgentVersionLength)},
		{"invalid UTF-8", "1.2.\xff", "1.2.�"},
		{"empty", "", "dev"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reportAgentVersion(tc.in)
			if got != tc.want {
				t.Fatalf("reportAgentVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}

			if n := utf8.RuneCountInString(got); n > MaxAgentVersionLength || !utf8.ValidString(got) {
				t.Fatalf("reportAgentVersion(%q) = %d characters, valid %v", tc.in, n, utf8.ValidString(got))
			}
		})
	}
}
