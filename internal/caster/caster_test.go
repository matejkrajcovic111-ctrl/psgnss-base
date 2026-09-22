package caster

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psgnss/psgnss-base/internal/rtcm"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

func TestUserAccessEnforcement(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "caster.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	kr, err := secrets.Generate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser(kr, "test-rover", "fixture-password", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Store: db, Keyring: kr, AllowV1: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if status, reason, err := c.checkUserAccess(u, "Test_MSM7", "not-an-ip", now); status != 0 || reason != "" || err != nil {
		t.Fatalf("unrestricted account refused: status=%d reason=%q err=%v", status, reason, err)
	}
	future := now.Add(time.Hour)
	edit := store.UserEdit{ConnectionLimit: 5, Enabled: true, ExpiresAt: &future,
		Access: store.UserAccess{IPs: []string{"192.0.2.0/24"}, Mountpoints: []string{"Test_MSM7"}}}
	if err := db.UpdateUser(nil, u.Username, edit); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUser(u.Username)
	for _, test := range []struct {
		mount, ip, reason string
		status            int
	}{
		{"Test_MSM7", "192.0.2.42", "", 0},
		{"Test_MSM4", "192.0.2.42", "mountpoint not allowed", http.StatusForbidden},
		{"Test_MSM7", "192.0.3.42", "IP not allowed", http.StatusForbidden},
	} {
		status, reason, err := c.checkUserAccess(u, test.mount, test.ip, now)
		if status != test.status || reason != test.reason || err != nil {
			t.Errorf("checkUserAccess(%q,%q)=%d,%q,%v want %d,%q,nil",
				test.mount, test.ip, status, reason, err, test.status, test.reason)
		}
	}
	past := now.Add(-time.Second)
	u.ExpiresAt = &past
	if status, reason, _ := c.checkUserAccess(u, "Test_MSM7", "192.0.2.42", now); status != http.StatusForbidden || reason != "expired" {
		t.Fatalf("expired account accepted: status=%d reason=%q", status, reason)
	}
	u.ExpiresAt = nil
	u.Enabled = false
	if status, reason, _ := c.checkUserAccess(u, "Test_MSM7", "192.0.2.42", now); status != http.StatusForbidden || reason != "disabled" {
		t.Fatalf("disabled account accepted: status=%d reason=%q", status, reason)
	}
	u.Enabled = true
	if _, err := db.DB().Exec(`UPDATE user_ip_rules SET cidr='corrupt' WHERE user_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if status, reason, err := c.checkUserAccess(u, "Test_MSM7", "192.0.2.42", now); status != http.StatusServiceUnavailable || reason != "IP policy evaluation failed" || err == nil {
		t.Fatalf("corrupt policy did not fail closed: status=%d reason=%q err=%v", status, reason, err)
	}
}

func TestSourcetableFieldOrder(t *testing.T) {
	m := MountEntry{
		Name: "Example_MSM7", SourceID: 1, Format: "RTCM 3.2",
		Messages: "1006(10),1077(1)", Carrier: 3, NavSystem: "GPS+GAL+BDS",
		Network: "example.org", Country: "SVK",
		Latitude: 48.12345678, Longitude: 17.98765432,
		Generator: "PSGNSS", Auth: "B", Fee: "N", Bitrate: 7220,
	}
	got := m.STR()
	f := strings.Split(got, ";")
	// STR + 17 fields + trailing empty from the final semicolon.
	if len(f) != 19 {
		t.Fatalf("STR has %d fields, want 19: %q", len(f), got)
	}
	for i, want := range map[int]string{
		0: "STR", 1: "Example_MSM7", 2: "1", 3: "RTCM 3.2",
		5: "3", 6: "GPS+GAL+BDS", 8: "SVK", 14: "none", 15: "B", 16: "N",
	} {
		if f[i] != want {
			t.Errorf("field %d = %q, want %q", i, f[i], want)
		}
	}
	// Coordinates must be published at 2 dp, not full precision.
	if f[9] != "48.12" || f[10] != "17.99" {
		t.Errorf("coordinates = %q/%q, want 48.12/17.99 (2 dp only)", f[9], f[10])
	}
}

func TestNetEntryDoesNotLeakInternalAddress(t *testing.T) {
	n := NetEntry{Network: "PSGNSS", Operator: "example.org", Auth: "B", Fee: "N"}
	got := n.NET()
	for _, bad := range []string{"100.64.", "192.168.", "10.", ":2305"} {
		if strings.Contains(got, bad) {
			t.Errorf("NET record leaks internal address %q: %s", bad, got)
		}
	}
}

func TestSourcetableTerminates(t *testing.T) {
	st := Sourcetable([]MountEntry{{Name: "A"}, {Name: "B"}}, nil)
	if !strings.HasSuffix(st, "ENDSOURCETABLE\r\n") {
		t.Errorf("sourcetable must end with ENDSOURCETABLE: %q", st)
	}
	if strings.Index(st, "STR;A") > strings.Index(st, "STR;B") {
		t.Error("mountpoints should be sorted by name")
	}
}

func TestBasicAuth(t *testing.T) {
	for _, c := range []struct {
		hdr, u, p string
		ok        bool
	}{
		{"Basic TWF0ZWo6MTIz", "Matej", "123", true},
		{"basic TWF0ZWo6MTIz", "Matej", "123", true}, // case-insensitive scheme
		{"Bearer xyz", "", "", false},
		{"", "", "", false},
		{"Basic !!!notbase64", "", "", false},
	} {
		u, p, ok := basicAuth(c.hdr)
		if ok != c.ok || (ok && (u != c.u || p != c.p)) {
			t.Errorf("basicAuth(%q) = %q,%q,%v want %q,%q,%v", c.hdr, u, p, ok, c.u, c.p, c.ok)
		}
	}
}

func TestParseRequestNtripVersion(t *testing.T) {
	for _, c := range []struct {
		raw  string
		ver  int
		meth string
		path string
	}{
		{"GET /Example_MSM7 HTTP/1.0\r\nUser-Agent: NTRIP x\r\n\r\n", 1, "GET", "/Example_MSM7"},
		{"GET /M HTTP/1.1\r\nNtrip-Version: Ntrip/2.0\r\n\r\n", 2, "GET", "/M"},
		{"SOURCE secret /Push1\r\n\r\n", 1, "SOURCE", "/Push1"},
	} {
		r, err := readRequest(bufio.NewReader(strings.NewReader(c.raw)))
		if err != nil {
			t.Fatalf("readRequest(%q): %v", c.raw, err)
		}
		if r.NtripVersion != c.ver || r.Method != c.meth || r.Path != c.path {
			t.Errorf("got v%d %s %s, want v%d %s %s",
				r.NtripVersion, r.Method, r.Path, c.ver, c.meth, c.path)
		}
	}
}

func TestSourceLineCarriesPassword(t *testing.T) {
	r, err := readRequest(bufio.NewReader(strings.NewReader("SOURCE hunter2 /Push1\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if r.SourcePass != "hunter2" {
		t.Errorf("SourcePass = %q, want hunter2", r.SourcePass)
	}
}

// TestProxyHeaderRequiresTrust is the security-critical case: an untrusted peer
// must not be able to forge its own origin address.
func TestProxyHeaderRequiresTrust(t *testing.T) {
	raw := "PROXY TCP4 203.0.113.9 10.0.0.1 51234 2101\r\nGET / HTTP/1.0\r\n\r\n"

	trust, _ := NewTrustList([]string{"10.0.0.0/8"})
	untrusted := &net.TCPAddr{IP: net.ParseIP("198.51.100.5"), Port: 9999}
	br := bufio.NewReader(strings.NewReader(raw))
	pi, err := ReadProxyHeader(br, untrusted, trust)
	if err != nil {
		t.Fatal(err)
	}
	if pi.Used {
		t.Error("PROXY header from an UNTRUSTED peer was honoured; addresses could be forged")
	}

	trusted := &net.TCPAddr{IP: net.ParseIP("10.0.0.4"), Port: 9999}
	br = bufio.NewReader(strings.NewReader(raw))
	pi, err = ReadProxyHeader(br, trusted, trust)
	if err != nil {
		t.Fatal(err)
	}
	if !pi.Used || pi.SrcIP != "203.0.113.9" || pi.SrcPort != 51234 {
		t.Errorf("trusted PROXY header not parsed: %+v", pi)
	}
}

func TestProxyV2Parsing(t *testing.T) {
	var b bytes.Buffer
	b.Write(v2Sig)
	b.WriteByte(0x21) // version 2, PROXY command
	b.WriteByte(0x11) // TCP over IPv4
	b.Write([]byte{0x00, 12})
	b.Write([]byte{203, 0, 113, 9}) // src
	b.Write([]byte{10, 0, 0, 1})    // dst
	b.Write([]byte{0xC8, 0x2A})     // src port 51242
	b.Write([]byte{0x08, 0x35})     // dst port
	b.WriteString("GET / HTTP/1.0\r\n\r\n")

	trust, _ := NewTrustList([]string{"10.0.0.4"})
	peer := &net.TCPAddr{IP: net.ParseIP("10.0.0.4"), Port: 1}
	pi, err := ReadProxyHeader(bufio.NewReader(&b), peer, trust)
	if err != nil {
		t.Fatal(err)
	}
	if !pi.Used || pi.SrcIP != "203.0.113.9" || pi.SrcPort != 51242 {
		t.Errorf("PROXY v2 parse = %+v, want 203.0.113.9:51242", pi)
	}
}

func TestNoProxyHeaderIsFine(t *testing.T) {
	trust, _ := NewTrustList([]string{"10.0.0.0/8"})
	peer := &net.TCPAddr{IP: net.ParseIP("10.0.0.4"), Port: 1}
	br := bufio.NewReader(strings.NewReader("GET / HTTP/1.0\r\n\r\n"))
	pi, err := ReadProxyHeader(br, peer, trust)
	if err != nil {
		t.Fatalf("plain request from a trusted peer must not error: %v", err)
	}
	if pi.Used {
		t.Error("reported a PROXY header where none was sent")
	}
	// The request must still be readable afterwards.
	r, err := readRequest(br)
	if err != nil || r.Path != "/" {
		t.Errorf("request consumed by proxy parsing: %v %+v", err, r)
	}
}

// TestBrowserDetection guards the split between a person with a browser and an
// NTRIP client. Answering a browser with "SOURCETABLE 200 OK" is not valid
// HTTP and shows up as an invalid response.
func TestBrowserDetection(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		browser bool
	}{
		{"chrome", "GET / HTTP/1.1\r\nAccept: text/html,application/xhtml+xml\r\n" +
			"User-Agent: Mozilla/5.0\r\n\r\n", true},
		{"ntrip v1 client", "GET / HTTP/1.0\r\nUser-Agent: NTRIP u-center\r\n\r\n", false},
		{"ntrip v2 client", "GET / HTTP/1.1\r\nNtrip-Version: Ntrip/2.0\r\n" +
			"Accept: text/html\r\n\r\n", false},
		{"bare client", "GET / HTTP/1.0\r\n\r\n", false},
		{"curl", "GET / HTTP/1.1\r\nAccept: */*\r\nUser-Agent: curl/8\r\n\r\n", false},
	}
	for _, c := range cases {
		r, err := readRequest(bufio.NewReader(strings.NewReader(c.raw)))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := isBrowser(r); got != c.browser {
			t.Errorf("%s: isBrowser = %v, want %v", c.name, got, c.browser)
		}
	}
}

// TestGeneratedMessagesUseConfiguredStationID pins the station-ID contract.
//
// At cutover the generated station messages carried ID 1 while the receiver's
// MSM output carried 0. Rovers received data and tracked satellites but never
// resolved, because observations cannot be tied to a base position when the
// reference station IDs disagree. Both sides must always match.
func TestGeneratedMessagesUseConfiguredStationID(t *testing.T) {
	for _, id := range []uint16{0, 1, 4095} {
		st := rtcm.Station{ID: id, AntennaDescriptor: "ADVNULLANTENNA", GPS: true}
		for name, gen := range map[string]func() ([]byte, error){
			"1006": st.Encode1006, "1008": st.Encode1008, "1033": st.Encode1033,
		} {
			f, err := gen()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			p := f[3 : len(f)-3]
			got := uint16(p[1]&0x0F)<<8 | uint16(p[2])
			if got != id {
				t.Errorf("%s: station ID = %d, want %d", name, got, id)
			}
		}
	}
}
