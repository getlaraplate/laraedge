//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyReload feeds reload on every SIGHUP until the returned stop is
// called; stop waits for the forwarding goroutine.
func notifyReload(reload chan<- struct{}) (stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-sig:
				select {
				case reload <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(sig)
		close(done)
		<-finished
	}
}
