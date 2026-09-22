package pushout

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

type mountsOf struct{ m []*caster.Mount }

func (x mountsOf) Mounts() []*caster.Mount { return x.m }

func testStore(t *testing.T) (*store.Store, *secrets.Keyring) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key := filepath.Join(dir, "master.key")
	kr, _, err := secrets.LoadOrGenerate(key)
	if err != nil {
		t.Fatal(err)
	}
	return db, kr
}

// fakeCaster accepts one upload, records the request line and headers, and
// streams whatever body arrives to bodies.
type fakeCaster struct {
	ln      net.Listener
	request chan string
	body    chan []byte
	reply   string
}

func newFakeCaster(t *testing.T, reply string) *fakeCaster {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCaster{ln: ln, request: make(chan string, 4), body: make(chan []byte, 64), reply: reply}
	go f.accept()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeCaster) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			br := bufio.NewReader(conn)
			var head strings.Builder
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				head.WriteString(line)
				if strings.TrimSpace(line) == "" {
					break
				}
			}
			f.request <- head.String()
			if _, err := conn.Write([]byte(f.reply)); err != nil {
				return
			}
			buf := make([]byte, 4096)
			for {
				n, err := br.Read(buf)
				if n > 0 {
					cp := make([]byte, n)
					copy(cp, buf[:n])
					select {
					case f.body <- cp:
					default:
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

func (f *fakeCaster) addr() string { return f.ln.Addr().String() }

// mountFed returns a caster mountpoint plus a function that pushes one frame
// through it, without running a whole caster.
func mountFed(t *testing.T, db *store.Store) (*caster.Mount, func([]byte)) {
	t.Helper()
	c, err := caster.New(caster.Options{Listen: "", Hub: &hub.Hub{}, Store: db, AllowV1: true, AllowV2: true,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m := c.AddMount(caster.MountEntry{Name: "Test_MSM7"}, hub.NewPassthrough())
	return m, m.Publish
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPushV1Handshake(t *testing.T) {
	db, kr := testStore(t)
	f := newFakeCaster(t, "ICY 200 OK\r\n\r\n")
	enc, nonce, err := kr.Seal("secretpw")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SavePushTargets([]store.PushTarget{{Name: "net", Enabled: true, Source: "Test_MSM7",
		Host: f.addr(), Mountpoint: "REMOTE1", Protocol: store.ProtocolV1,
		PasswordEnc: enc, PasswordNonce: nonce}}); err != nil {
		t.Fatal(err)
	}
	m, feed := mountFed(t, db)
	mgr := New(db, kr, mountsOf{[]*caster.Mount{m}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	var req string
	select {
	case req = <-f.request:
	case <-time.After(5 * time.Second):
		t.Fatal("caster never received a request")
	}
	if !strings.HasPrefix(req, "SOURCE secretpw /REMOTE1\r\n") {
		t.Errorf("unexpected v1 request line: %q", req)
	}
	if !strings.Contains(req, "Source-Agent: NTRIP PSGNSS/") {
		t.Errorf("v1 request has no Source-Agent: %q", req)
	}

	waitFor(t, "connected state", func() bool {
		st := mgr.Status()
		return len(st) == 1 && st[0].State == "connected"
	})

	feed([]byte{0xD3, 0x00, 0x01, 0xFF, 0x00, 0x00, 0x00})
	select {
	case got := <-f.body:
		// v1 is not chunked: the frame arrives verbatim.
		if got[0] != 0xD3 {
			t.Errorf("v1 body was framed: % x", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame never reached the caster")
	}
	waitFor(t, "byte accounting", func() bool { return mgr.Status()[0].BytesSent > 0 })
}

func TestPushV2HandshakeIsChunked(t *testing.T) {
	db, kr := testStore(t)
	f := newFakeCaster(t, "HTTP/1.1 200 OK\r\nServer: fake\r\n\r\n")
	enc, nonce, err := kr.Seal("pw2")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SavePushTargets([]store.PushTarget{{Name: "net2", Enabled: true, Source: "Test_MSM7",
		Host: f.addr(), Mountpoint: "REMOTE2", Protocol: store.ProtocolV2, Username: "user",
		PasswordEnc: enc, PasswordNonce: nonce}}); err != nil {
		t.Fatal(err)
	}
	m, feed := mountFed(t, db)
	mgr := New(db, kr, mountsOf{[]*caster.Mount{m}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)

	req := <-f.request
	for _, want := range []string{"POST /REMOTE2 HTTP/1.1", "Ntrip-Version: Ntrip/2.0",
		"Transfer-Encoding: chunked", "Authorization: Basic dXNlcjpwdzI="} {
		if !strings.Contains(req, want) {
			t.Errorf("v2 request missing %q in:\n%s", want, req)
		}
	}
	waitFor(t, "connected state", func() bool {
		st := mgr.Status()
		return len(st) == 1 && st[0].State == "connected"
	})
	feed([]byte{0xD3, 0x00, 0x01, 0xAA})
	select {
	case got := <-f.body:
		if !strings.HasPrefix(string(got), "4\r\n") {
			t.Errorf("v2 body is not chunked: % x", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame never reached the caster")
	}
}

func TestPushRefusalIsReportedAndRetried(t *testing.T) {
	db, kr := testStore(t)
	f := newFakeCaster(t, "ERROR - Bad Password\r\n")
	if err := db.SavePushTargets([]store.PushTarget{{Name: "bad", Enabled: true, Source: "Test_MSM7",
		Host: f.addr(), Mountpoint: "NOPE", Protocol: store.ProtocolV1}}); err != nil {
		t.Fatal(err)
	}
	m, _ := mountFed(t, db)
	mgr := New(db, kr, mountsOf{[]*caster.Mount{m}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	waitFor(t, "refusal to surface", func() bool {
		st := mgr.Status()
		return len(st) == 1 && strings.Contains(st[0].LastError, "Bad Password") && st[0].State == "retrying"
	})
	if got := mgr.Status()[0].BytesSent; got != 0 {
		t.Errorf("refused target sent %d bytes", got)
	}
}

func TestMissingLocalMountpointIsAnError(t *testing.T) {
	db, kr := testStore(t)
	if err := db.SavePushTargets([]store.PushTarget{{Name: "orphan", Enabled: true,
		Source: "Does_Not_Exist", Host: "127.0.0.1:1", Mountpoint: "X", Protocol: store.ProtocolV1}}); err != nil {
		t.Fatal(err)
	}
	mgr := New(db, kr, mountsOf{nil}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	waitFor(t, "missing mountpoint error", func() bool {
		st := mgr.Status()
		return len(st) == 1 && strings.Contains(st[0].LastError, "does not exist")
	})
}

func TestDisabledTargetDoesNotRun(t *testing.T) {
	db, kr := testStore(t)
	if err := db.SavePushTargets([]store.PushTarget{{Name: "off", Source: "Test_MSM7",
		Host: "127.0.0.1:1", Mountpoint: "X", Protocol: store.ProtocolV1}}); err != nil {
		t.Fatal(err)
	}
	m, _ := mountFed(t, db)
	mgr := New(db, kr, mountsOf{[]*caster.Mount{m}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if st := mgr.Status(); len(st) != 0 {
		t.Errorf("disabled target is running: %+v", st)
	}
}

func TestSavePushTargetsKeepsStoredPassword(t *testing.T) {
	db, kr := testStore(t)
	enc, nonce, err := kr.Seal("keepme")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SavePushTargets([]store.PushTarget{{Name: "keep", Source: "a", Host: "h:1",
		Mountpoint: "M", Protocol: store.ProtocolV2, PasswordEnc: enc, PasswordNonce: nonce}}); err != nil {
		t.Fatal(err)
	}
	// Re-save the same name with no password, as the UI does when only the
	// mountpoint changed.
	if err := db.SavePushTargets([]store.PushTarget{{Name: "keep", Source: "a", Host: "h:1",
		Mountpoint: "OTHER", Protocol: store.ProtocolV2}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.PushTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].HasPassword {
		t.Fatalf("password was discarded on edit: %+v", got)
	}
	if got[0].Mountpoint != "OTHER" {
		t.Errorf("edit did not apply: %+v", got[0])
	}
	pw, err := kr.Open(got[0].PasswordEnc, got[0].PasswordNonce)
	if err != nil || pw != "keepme" {
		t.Errorf("stored password no longer opens: %q %v", pw, err)
	}
}

func TestDuplicateTargetNamesRejected(t *testing.T) {
	db, kr := testStore(t)
	_ = kr
	err := db.SavePushTargets([]store.PushTarget{
		{Name: "dup", Source: "a", Host: "h:1", Mountpoint: "M", Protocol: store.ProtocolV1},
		{Name: "DUP", Source: "a", Host: "h:1", Mountpoint: "M", Protocol: store.ProtocolV1},
	})
	if err == nil {
		t.Fatal("duplicate names were accepted")
	}
}
