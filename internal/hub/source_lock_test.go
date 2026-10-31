package hub

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Two readers on one serial port each get a random share of its bytes, so
// both see a stream of broken frames. A second psgnssd started by mistake did
// exactly that to a live station; the second open must fail instead.
func TestSerialSourceIsExclusive(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no pty available:", err)
	}
	defer master.Close()
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip(err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip(err)
	}
	src := SerialSource{Device: "/dev/pts/" + strconv.Itoa(n), Baud: 115200}

	first, err := src.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Open(); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second open of a held port: err = %v, want refusal", err)
	}
	first.Close()
	again, err := src.Open()
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	again.Close()
}
