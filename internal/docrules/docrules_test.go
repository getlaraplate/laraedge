package docrules

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const plainToken = "plain-token-value-1234"

func TestCheckURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "https", raw: "https://mes.example.com"},
		{name: "https with port and path", raw: "https://mes.example.com:8443/base"},
		{name: "http on localhost", raw: "http://localhost:8000"},
		{name: "http on ipv4 loopback", raw: "http://127.0.0.1"},
		{name: "http on ipv6 loopback", raw: "http://[::1]:8000"},
		{name: "http on a remote host", raw: "http://mes.example.com", wantErr: "must use https (http is accepted only for a loopback host)"},
		{name: "relative", raw: "mes.example.com", wantErr: "must be an absolute URL"},
		{name: "empty", raw: "", wantErr: "must be an absolute URL"},
		{name: "unsupported scheme", raw: "ftp://mes.example.com", wantErr: "must use https"},
		{name: "credentials", raw: "https://user:" + plainToken + "@mes.example.com", wantErr: "must not contain credentials"},
		{name: "unparsable", raw: "https://" + plainToken + "%zz", wantErr: "must be an absolute URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckURL(tt.raw)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckURL = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CheckURL = %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), plainToken) {
				t.Errorf("error echoes the URL: %v", err)
			}
		})
	}
}

func TestIsSafeKey(t *testing.T) {
	for key, want := range map[string]bool{
		"agent_id": true, "max_samples": true, "sha256": true,
		"Plain-Token": false, "": false, "9lives": false, "a-b": false,
		"a234567890123456789012345678901x": true, "a234567890123456789012345678901xy": false,
	} {
		if got := IsSafeKey(key); got != want {
			t.Errorf("IsSafeKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestDescribeTypeErrorNeverEchoesUnrecognisedEntries(t *testing.T) {
	tests := []struct {
		entry string
		want  string
	}{
		{"line 3: cannot unmarshal !!str `plain-t` into int", "line 3: cannot unmarshal !!str into int"},
		{"line 3: cannot unmarshal !!str `a into b` into int", "line 3: cannot unmarshal !!str into int"},
		{`line 4: mapping key "agent_id" already defined at line 2`, "line 4: duplicate key agent_id"},
		{`line 4: mapping key "Plain-Token" already defined at line 2`, "line 4: duplicate key"},
		{"line 5: field Plain-Token not found in type bootstrap.File", "line 5: unknown key"},
		{"line 5: field extra not found in type bootstrap.Config", "line 5: unknown key extra in bootstrap.Config"},
		{"line 6: field Plain-Token already set in type bootstrap.File", "line 6: duplicate key"},
		{"line 7: Plain-Token something new", "line 7: invalid YAML"},
		{"Plain-Token without a line", "invalid YAML"},
	}
	for _, tt := range tests {
		if got := describeTypeError(tt.entry); got != tt.want {
			t.Errorf("describeTypeError(%q) = %q, want %q", tt.entry, got, tt.want)
		}
	}
}

func TestDescribeDecodeErrorNeverEchoesPlainYAMLErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{name: "unknown alias", doc: "a: *" + plainToken + "\n", want: "unknown alias"},
		{name: "self-referencing anchor", doc: "a: &" + plainToken + " [*" + plainToken + "]\n", want: "recursive anchor"},
		{name: "invalid map key", doc: "? [" + plainToken + "]\n: x\n", want: "invalid map key"},
		{name: "explicit tag", doc: "a: !!int " + plainToken + "\n", want: "value does not match its explicit tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v any
			err := yaml.Unmarshal([]byte(tt.doc), &v)
			if err == nil {
				t.Fatal("yaml accepted the document; the test no longer reproduces the library error")
			}
			if !strings.Contains(err.Error(), plainToken) {
				t.Fatalf("library error %q no longer echoes the document; revisit DescribeYAMLError", err)
			}
			got := DescribeYAMLError(err)
			if got != tt.want {
				t.Errorf("DescribeYAMLError = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, plainToken) || strings.Contains(got, plainToken[:7]) {
				t.Errorf("message echoes document text: %q", got)
			}
		})
	}
}

func TestDescribeDecodeErrorKeepsConstantSyntaxErrors(t *testing.T) {
	var v any
	err := yaml.Unmarshal([]byte("a: [unclosed\n"), &v)
	if err == nil {
		t.Fatal("yaml accepted the document")
	}
	if got := DescribeYAMLError(err); got != err.Error() {
		t.Errorf("DescribeYAMLError = %q, want %q", got, err.Error())
	}
}
