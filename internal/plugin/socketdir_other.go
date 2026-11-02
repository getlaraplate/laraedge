//go:build !unix

package plugin

import "path/filepath"

// socketDir returns the socket directory of plugin id: runDir/plugins/<id>
// when runDir is set, else dataDir/run/plugins/<id>. Named pipes have no
// path limit, so there is no fallback and nothing is checked.
func socketDir(dataDir, runDir, id string) (dir string, fallback bool, err error) {
	base := runDir
	if base == "" {
		base = filepath.Join(dataDir, "run")
	}

	return filepath.Join(base, "plugins", id), false, nil
}
