package hub

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY returns the follower side of a fresh pseudo-terminal. A pty keeps
// whatever line speed it is given, so it shows what the kernel really applied.
func openPTY(t *testing.T) *os.File {
	t.Helper()
	leader, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { leader.Close() })
	fd := int(leader.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	follower, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open follower: %v", err)
	}
	t.Cleanup(func() { follower.Close() })
	return follower
}

// The speed must reach the kernel. TCSETS reads it from the CBAUD bits of
// c_cflag, so setting only Ispeed/Ospeed leaves the port at whatever speed it
// had before — 9600 on a freshly plugged FTDI adapter.
func TestConfigureTTYAppliesBaud(t *testing.T) {
	for _, baud := range []int{9600, 115200, 460800, 115200} {
		f := openPTY(t)
		if err := configureTTY(f, baud); err != nil {
			t.Fatalf("configureTTY(%d): %v", baud, err)
		}
		got, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatalf("TCGETS: %v", err)
		}
		if got.Cflag&unix.CBAUD != baudRates[baud] {
			t.Errorf("baud %d: kernel has CBAUD %#o, want %#o", baud, got.Cflag&unix.CBAUD, baudRates[baud])
		}
		if got.Cflag&unix.CSIZE != unix.CS8 {
			t.Errorf("baud %d: not 8 data bits", baud)
		}
	}
}
