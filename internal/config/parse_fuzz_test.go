package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/getlaraplate/laraplate-edge/internal/config"
)

// FuzzParse checks that Parse never fails internally or panics, that it
// returns either a document or messages, and that an accepted document parses
// back from its Raw form to the same document.
func FuzzParse(f *testing.F) {
	for _, name := range []string{"spec-5.3.yaml", "spec-5.3.json", "build.yaml", "build.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(base))
	f.Add([]byte(`{"schema":"laraplate-edge-config/1","inputs":[{"id":7,"devices":"x"}],"outputs":[{"batch":3,"buffer":[],"route":{"include":5}}]}`))
	f.Add([]byte("a: &x [*x, *x]\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		doc, msgs, err := config.Parse(data)
		if err != nil {
			t.Fatalf("internal error: %v", err)
		}
		if len(msgs) > 0 {
			if !reflect.DeepEqual(doc, config.Document{}) {
				t.Fatal("document not zero with messages")
			}
			return
		}

		again, msgs, err := config.Parse(doc.Raw)
		if err != nil || len(msgs) != 0 {
			t.Fatalf("Raw does not parse: %q, %v\n%s", msgs, err, doc.Raw)
		}
		if !reflect.DeepEqual(doc, again) {
			t.Fatalf("Raw parses to another document:\n%s\n%s", doc.Raw, again.Raw)
		}
		_ = config.CheckBuild(doc)
	})
}
