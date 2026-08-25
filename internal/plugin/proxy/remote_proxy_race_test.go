package proxy

import (
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// TestRemoteProxy_CallbackRace exercises the data race between the OnXxx
// callback setters (caller goroutine) and handleNotification reading those
// same fields (read-pump goroutine). OpenRemote starts the read pump before
// the callbacks are registered, and remote.* notifications are asynchronous,
// so the two genuinely interleave. Run with -race: pre-fix the unsynchronized
// onConnState field tripped the detector; it must be clean after the fix.
func TestRemoteProxy_CallbackRace(t *testing.T) {
	p := &RemoteProxy{}
	frame := []byte(`{"params":{"conn_state":"connected","human_status":"ok"}}`)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			p.OnConnState(func(state, human string) {})
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ {
		p.handleNotification(proto.NotificationRemoteConnState, frame)
	}
	<-done
}
