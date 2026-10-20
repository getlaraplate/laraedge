// Package version exposes the agent version baked in at build time.
package version

// version is set at link time:
//
//	go build -ldflags "-X github.com/getlaraplate/laraplate-edge/internal/version.version=1.2.3"
var version = "dev"

// String returns the agent version, or "dev" for an unversioned build.
func String() string {
	return version
}
