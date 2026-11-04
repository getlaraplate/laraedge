package opcua

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A rename opens its source by name for DELETE, sharing everything, and
// renames through that handle. Such a handle opened on own-<n>.tmp survives
// another sweep's claim, and settleClaim does not let the claim proceed
// while it is open: once it is closed the claim is settled.
func TestSettleClaimWaitsForARenameInFlight(t *testing.T) {
	pki := t.TempDir()
	path := filepath.Join(pki, "own-1.tmp")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	inFlight, err := windows.CreateFile(name, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(inFlight)
		}
	}()

	claimed := filepath.Join(pki, "own-1.tmp.sweep-0000000000000001-0123456789abcdef")
	if err := os.Rename(path, claimed); err != nil {
		t.Fatalf("claim with a rename in flight: %v", err)
	}

	if present, err := settleClaim(claimed); err == nil || present || !strings.Contains(err.Error(), "another handle") {
		t.Fatalf("settleClaim with a rename in flight = %v, %v; want the open handle named", present, err)
	}

	if err := windows.CloseHandle(inFlight); err != nil {
		t.Fatal(err)
	}
	closed = true

	if present, err := settleClaim(claimed); err != nil || !present {
		t.Fatalf("settleClaim = %v, %v; want present", present, err)
	}

	if present, err := settleClaim(filepath.Join(pki, "gone")); err != nil || present {
		t.Fatalf("settleClaim(gone) = %v, %v; want absent and no error", present, err)
	}
}

// A data directory beyond MAX_PATH (260 characters) is settled like any
// other: the path is opened in its \\?\ form, never read as gone.
func TestSettleClaimTakesALongPath(t *testing.T) {
	claimed := filepath.Join(t.TempDir(), strings.Repeat("d", 120), strings.Repeat("e", 120), "own-1.tmp.sweep-0000000000000001-0123456789abcdef")
	if err := os.MkdirAll(claimed, 0o700); err != nil {
		t.Fatal(err)
	}

	if present, err := settleClaim(claimed); err != nil || !present {
		t.Fatalf("settleClaim(%d characters) = %v, %v; want present", len(claimed), present, err)
	}
}
