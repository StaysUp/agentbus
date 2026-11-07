//go:build integration

package gateway

import (
	"net/url"
	"testing"
	"time"

	"github.com/staysup/agentbus/adapter/anthropic"
	"github.com/staysup/agentbus/event"
)

// The timeout's wakeup must not be lost when it fires before Settle waits.
// Many short timeouts on a turn that never settles must each return false
// promptly.
func TestSettleTimeoutWakeupIsNeverLost(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	g := New(u, anthropic.Adapter{}, &event.Memory{})
	t.Cleanup(g.Close)
	g.track(&exchange{}, "s1", "t_stuck") // never released
	for i := 0; i < 3000; i++ {
		done := make(chan bool, 1)
		timeout := time.Duration(i%50) * time.Microsecond
		go func() { done <- g.Settle("s1", "t_stuck", timeout) }()
		select {
		case ok := <-done:
			if ok {
				t.Fatal("settled a turn with an open call")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Settle with timeout %v did not return: its wakeup was lost", timeout)
		}
	}
}
