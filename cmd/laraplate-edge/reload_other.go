//go:build !unix

package main

// notifyReload does nothing where there is no SIGHUP (Windows): the
// bootstrap is reloaded by restarting the service.
func notifyReload(chan<- struct{}) (stop func()) {
	return func() {}
}
