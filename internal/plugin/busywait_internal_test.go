package plugin

import (
	"testing"

	"github.com/getlaraplate/laraedge/internal/input/driverkit"
)

// The supervisor tests count a Sleep of at least driverkit.BackoffMin/2 as a
// host restart wait; a busy wait at or above that would be miscounted.
func TestBusyWaitStaysBelowBackoffFloor(t *testing.T) {
	if busyWait >= driverkit.BackoffMin/2 {
		t.Fatalf("busyWait = %v, want below %v", busyWait, driverkit.BackoffMin/2)
	}
}
