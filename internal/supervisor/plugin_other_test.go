//go:build !unix

package supervisor

// alive reports false: outside Unix the tests rely on the status, which
// lists a plugin with its pid only while it is up.
func alive(int) bool { return false }
