//go:build !windows

package opcua

// settleClaim reports the claimed directory present: outside Windows a rename
// resolves the source name and moves it in one system call, so once the
// claim's rename has returned no other sweep can still move the directory.
// The Lstat after it finds one that is gone.
func settleClaim(string) (present bool, err error) {
	return true, nil
}
