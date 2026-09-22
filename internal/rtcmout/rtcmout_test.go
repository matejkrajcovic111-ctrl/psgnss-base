package rtcmout

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/store"
)

type mountsOf struct{ m []*caster.Mount }

func (x mountsOf) Mounts() []*caster.Mount { return x.m }

func testMount(t *testing.T) (*caster.Mount, func([]byte)) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/out.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := caster.New(caster.Options{Listen: "", Hub: &hub.Hub{}, Store: db,
		AllowV1: true, AllowV2: true, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m := c.AddMount(caster.MountEntry{Name: "Test_MSM7"}, hub.NewPassthrough())
	return m, m.Publish
}

func TestUDPSinkSendsOneDatagramPerFrame(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	m, feed := testMount(t)
	mgr := New([]config.RTCMOut{{Name: "radio-lan", Kind: "udp", Source: "Test_MSM7",
		Target: pc.LocalAddr().String()}}, mountsOf{[]*caster.Mount{m}},
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && mgr.Status()[0].State != "sending" {
		time.Sleep(10 * time.Millisecond)
	}
	if st := mgr.Status()[0]; st.State != "sending" {
		t.Fatalf("sink did not start: %+v", st)
	}
	// Two frames must arrive as two datagrams, with their boundaries intact.
	feed([]byte{0xD3, 0x00, 0x03, 0x01, 0x02, 0x03})
	feed([]byte{0xD3, 0x00, 0x02, 0x09, 0x08})
	buf := make([]byte, 1500)
	for i, want := range []int{6, 5} {
		if err := pc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		if n != want {
			t.Errorf("datagram %d is %d bytes, want %d: frame boundaries were not preserved", i, n, want)
		}
	}
}

func TestSerialSinkWritesToTheDevice(t *testing.T) {
	// A pty stands in for a radio: it is a real tty, so the termios setup runs
	// for real, which a plain file would skip.
	master, slaveName, err := openPTY()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer master.Close()
	m, feed := testMount(t)
	mgr := New([]config.RTCMOut{{Name: "radio", Kind: "serial", Source: "Test_MSM7",
		Device: slaveName, Baud: 115200}}, mountsOf{[]*caster.Mount{m}},
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && mgr.Status()[0].State != "sending" {
		time.Sleep(10 * time.Millisecond)
	}
	if st := mgr.Status()[0]; st.State != "sending" {
		t.Fatalf("serial sink did not start: %+v", st)
	}
	payload := []byte{0xD3, 0x00, 0x04, 0x01, 0x02, 0x03, 0x04}
	feed(payload)
	if err := master.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Read the whole frame rather than whatever one read happens to return: a
	// pty may hand back a short read and the assertion below is about the
	// bytes, not about how they were delivered.
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(master, buf); err != nil {
		t.Fatal(err)
	}
	// Raw mode must not have translated anything.
	if string(buf) != string(payload) {
		t.Errorf("read % x, want % x", buf, payload)
	}
	// The accounting is incremented after the write returns, so arriving bytes
	// can be observed before the counter moves. Waiting for it is the test's
	// job; making the write and the count atomic would be a lock on the hot
	// path for a display figure.
	countDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(countDeadline) && mgr.Status()[0].BytesSent != int64(len(payload)) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := mgr.Status()[0]; st.BytesSent != int64(len(payload)) {
		t.Errorf("bytes sent = %d, want %d", st.BytesSent, len(payload))
	}
}

func TestMissingMountpointAndUnknownKindAreReported(t *testing.T) {
	m, _ := testMount(t)
	mgr := New([]config.RTCMOut{
		{Name: "orphan", Kind: "udp", Source: "Nope", Target: "127.0.0.1:1"},
		{Name: "weird", Kind: "carrier-pigeon", Source: "Test_MSM7"},
	}, mountsOf{[]*caster.Mount{m}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := mgr.Status()
		if strings.Contains(st[0].LastError, "does not exist") &&
			strings.Contains(st[1].LastError, "unknown output kind") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("errors not reported: %+v", mgr.Status())
}

func TestDisabledSinkIsNotStarted(t *testing.T) {
	m, _ := testMount(t)
	mgr := New([]config.RTCMOut{{Name: "off", Disabled: true, Kind: "udp",
		Source: "Test_MSM7", Target: "127.0.0.1:1"}}, mountsOf{[]*caster.Mount{m}},
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if got := len(mgr.Status()); got != 0 {
		t.Errorf("disabled sink is running: %d", got)
	}
}
