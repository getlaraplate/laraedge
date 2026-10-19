// Package e2e holds the end-to-end test of the agent: the built binary runs
// the simulated driver against a fake Laraplate backend that validates every
// envelope against the backend schema, through a network cut, a kill -9 with
// a restart, and a configuration change served by the laraplate provider.
//
// The test is behind the e2e build tag, Unix only:
//
//	go test -tags e2e -race ./internal/e2e
package e2e
