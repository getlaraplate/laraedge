package opcua

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

const (
	// settleAttempts bounds how often settleClaim looks for a rename in
	// flight; one finishes within microseconds of opening its handle.
	settleAttempts = 20
	// settleWait is the pause between two of those looks.
	settleWait = 5 * time.Millisecond
)

// settleClaim returns once no other rename can move the directory claimed at
// path: present is true when it is still there, false when a rename in
// flight moved it away (that sweep claimed it and handles it).
//
// On Windows a rename is not one step on a name: it opens the source by name
// with DELETE access, sharing read, write and delete, then renames the open
// file through the handle (FileRenameInformation). A sweep that opened
// own-<n>.tmp before this sweep claimed it can still rename the directory,
// now under this sweep's claimed name, to its own claim, at any time until it
// closes that handle. An open of path for DELETE sharing nothing succeeds only
// while no such handle is open, and none can be opened later: the name it
// was opened by is gone and other sweeps leave a fresh claim alone.
func settleClaim(path string) (present bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}

	for attempt := 1; ; attempt++ {
		h, err := windows.CreateFile(name, windows.DELETE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err == nil {
			return true, windows.CloseHandle(h)
		}

		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}

		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return false, err
		}

		if attempt == settleAttempts {
			return false, fmt.Errorf("another handle stays open on it: %w", err)
		}

		time.Sleep(settleWait)
	}
}
