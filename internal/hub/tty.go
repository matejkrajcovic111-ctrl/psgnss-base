package hub

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// baudRates maps a rate to its termios constant. Only rates the ArduSimple
// boards actually use are listed; anything else is rejected loudly rather than
// silently falling back to a wrong speed.
var baudRates = map[int]uint32{
	9600: unix.B9600, 19200: unix.B19200, 38400: unix.B38400,
	57600: unix.B57600, 115200: unix.B115200, 230400: unix.B230400,
	460800: unix.B460800, 921600: unix.B921600,
}

// ConfigureTTY puts a port into raw 8N1 at the given baud. Outbound serial
// links (an RTCM radio) need exactly the same treatment as the receiver's port,
// so this is exported rather than copied.
func ConfigureTTY(f *os.File, baud int) error { return configureTTY(f, baud) }

// configureTTY puts the port into raw 8N1 at the given baud.
//
// A USB CDC-ACM device ignores the line speed, but a USB-serial bridge such as
// the FTDI on the UM980 board does not: it comes up at 9600 after every boot.
// TCSETS takes the speed from the CBAUD bits of c_cflag and ignores the
// Ispeed/Ospeed fields, so the speed must go into Cflag.
//
// The raw-mode flags matter just as much: without them the kernel would
// translate CR/LF and interpret control characters, corrupting binary UBX and
// RTCM payloads.
func configureTTY(f *os.File, baud int) error {
	speed, ok := baudRates[baud]
	if !ok {
		return fmt.Errorf("unsupported baud %d", baud)
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("TCGETS: %w", err)
	}
	// Raw mode: no canonical processing, no echo, no signals, no translation.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | speed
	t.Ispeed, t.Ospeed = speed, speed
	// Block until at least one byte is available; no inter-byte timer.
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("TCSETS: %w", err)
	}
	return nil
}
