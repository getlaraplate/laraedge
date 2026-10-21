package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/getlaraplate/laraedge/internal/config"
)

func TestStoreLastValidWhenNothingSaved(t *testing.T) {
	store := config.NewStore(t.TempDir())

	doc, ok, err := store.LastValid()
	if err != nil || ok || !reflect.DeepEqual(doc, config.Document{}) {
		t.Fatalf("LastValid = %+v, %v, %v; want zero, false, nil", doc, ok, err)
	}
}

func TestStoreSaveValidRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	store := config.NewStore(dataDir)

	for _, fixture := range []string{"spec-5.3.yaml", "build.yaml"} {
		want := mustParse(t, readFixture(t, fixture))

		if err := store.SaveValid(want); err != nil {
			t.Fatalf("SaveValid(%s): %v", fixture, err)
		}

		got, ok, err := config.NewStore(dataDir).LastValid()
		if err != nil || !ok {
			t.Fatalf("LastValid after %s = %v, %v", fixture, ok, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("LastValid after %s:\n%s\nwant\n%s", fixture, got.Raw, want.Raw)
		}
	}

	path := filepath.Join(dataDir, "config", "last-valid.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}

	leftovers, err := filepath.Glob(filepath.Join(dataDir, "config", "*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Errorf("temporary files left: %v, %v", leftovers, err)
	}
}

func TestStoreRefusesADocumentWithoutRaw(t *testing.T) {
	store := config.NewStore(t.TempDir())

	if err := store.SaveValid(config.Document{}); err == nil {
		t.Fatal("SaveValid accepted a document that did not come from Parse")
	}

	if _, ok, err := store.LastValid(); ok || err != nil {
		t.Fatalf("LastValid = %v, %v after a refused save", ok, err)
	}
}

func TestStoreCorruptFileIsAnError(t *testing.T) {
	tests := map[string]string{
		"not JSON":       `{"schema":`,
		"invalid":        `{"schema":"laraplate-edge-config/1","inputs":[],"outputs":[{"id":"x","type":"laraplate-http","token":"plain-token-value"}]}`,
		"empty":          ``,
		"not an object":  `[]`,
		"wrong encoding": "\xff\xfe",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			dir := filepath.Join(dataDir, "config")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "last-valid.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			doc, ok, err := config.NewStore(dataDir).LastValid()
			if err == nil || ok || !reflect.DeepEqual(doc, config.Document{}) {
				t.Fatalf("LastValid = %+v, %v, %v; want an error", doc, ok, err)
			}
			if strings.Contains(err.Error(), "plain-token-value") {
				t.Errorf("error echoes a value: %v", err)
			}
		})
	}
}

func TestStoreSaveFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := config.NewStore(dataDir).SaveValid(mustParse(t, []byte(base))); err == nil {
		t.Fatal("SaveValid succeeded with a file in place of the config directory")
	}
}
