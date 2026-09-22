package rtcmout

import (
	"os"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pty pair and returns the master plus the slave's path, so
// a test can point a serial sink at something that behaves like a real tty.
func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	name, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", err
	}
	return master, "/dev/pts/" + itoa(name), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
