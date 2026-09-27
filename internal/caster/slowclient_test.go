package caster

import (
	"net"
	"testing"
	"time"
)

// A rover that stops reading is dropped from the broadcast. Dropping it used
// to close its queue while it was still registered, so the very next frame
// was sent on a closed channel and the panic took the whole caster down with
// every other rover on it.
func TestSlowClientIsDroppedWithoutPanic(t *testing.T) {
	m := &Mount{clients: make(map[*client]struct{}), last: make(map[int][]byte)}
	server, peer := net.Pipe() // nobody reads peer: the rover has stalled
	defer peer.Close()
	cl := newClient(server, 2)
	m.clients[cl] = struct{}{}

	for i := 0; i < 50; i++ {
		m.Publish([]byte{byte(i)})
		time.Sleep(time.Millisecond) // let an asynchronous close land between frames
	}
	select {
	case <-cl.gone:
	default:
		t.Fatal("a client that stopped reading was never dropped")
	}
	if !cl.slow.Load() {
		t.Fatal("the drop was not attributed to the client being too slow")
	}
}
