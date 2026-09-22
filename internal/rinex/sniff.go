// Package rinex converts raw archive files to RINEX using RTKLIB convbin.
package rinex

import (
	"fmt"
	"io"
	"os"
)

// Content describes what a raw archive file actually holds.
type Content struct {
	UBXBytes   int64
	RTCMBytes  int64
	OtherBytes int64
	Total      int64
	RXMRawx    int64
	RXMSfrbx   int64
}

// HasUBX reports whether the file carries UBX, which is what convbin needs to
// produce observations and navigation data.
func (c Content) HasUBX() bool { return c.UBXBytes > 0 }

// HasNavigation reports whether the UBX stream contains both the broadcast
// subframes and a timed raw-measurement epoch. RTKLIB cannot date SFRBX-only
// data, so merely finding UBX framing is not enough to promise a RINEX NAV
// download.
func (c Content) HasNavigation() bool { return c.RXMRawx > 0 && c.RXMSfrbx > 0 }

// Format returns the convbin -r argument for this file.
func (c Content) Format() string {
	if c.UBXBytes >= c.RTCMBytes {
		return "ubx"
	}
	return "rtcm3"
}

func (c Content) String() string {
	if c.Total == 0 {
		return "empty"
	}
	return fmt.Sprintf("ubx %.0f%%, rtcm3 %.0f%%, other %.0f%%",
		100*float64(c.UBXBytes)/float64(c.Total),
		100*float64(c.RTCMBytes)/float64(c.Total),
		100*float64(c.OtherBytes)/float64(c.Total))
}

// Sniff classifies a raw file by protocol framing.
//
// This exists because the archive changes shape at cutover. Files written
// before it are the full multiplexed stream (UBX + RTCM). Files PSGNSS writes
// under the same historical naming hold RTCM only, with UBX in its own
// archive. Rather than assume a date boundary -- which would be wrong for any
// file copied, backfilled or restored out of order -- the converter reads the
// file and decides from its actual contents.
func Sniff(path string, limit int64) (Content, error) {
	f, err := os.Open(path)
	if err != nil {
		return Content{}, err
	}
	defer f.Close()
	if limit <= 0 {
		limit = 1 << 20 // 1 MiB is ~2 minutes of stream: ample to classify
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return Content{}, err
	}
	return classify(buf[:n]), nil
}

// SniffEdges classifies the beginning and end of a file. This matters for a
// live archive after its filter is upgraded: the first bytes can contain the
// old message set while the tail already contains the complete one.
func SniffEdges(path string, limit int64) (Content, error) {
	f, err := os.Open(path)
	if err != nil {
		return Content{}, err
	}
	defer f.Close()
	if limit <= 0 {
		limit = 64 << 10
	}
	fi, err := f.Stat()
	if err != nil {
		return Content{}, err
	}
	readAt := func(off int64) (Content, error) {
		buf := make([]byte, limit)
		n, err := f.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			return Content{}, err
		}
		return classify(buf[:n]), nil
	}
	head, err := readAt(0)
	if err != nil || fi.Size() <= limit {
		return head, err
	}
	tail, err := readAt(fi.Size() - limit)
	if err != nil {
		return Content{}, err
	}
	head.UBXBytes += tail.UBXBytes
	head.RTCMBytes += tail.RTCMBytes
	head.OtherBytes += tail.OtherBytes
	head.Total += tail.Total
	head.RXMRawx += tail.RXMRawx
	head.RXMSfrbx += tail.RXMSfrbx
	return head, nil
}

func classify(b []byte) Content {
	var c Content
	i := 0
	for i < len(b) {
		switch {
		case b[i] == 0xD3 && i+3 <= len(b):
			ln := int(b[i+1]&0x03)<<8 | int(b[i+2])
			total := 3 + ln + 3
			if i+total <= len(b) && crc24q(b[i:i+3+ln]) ==
				uint32(b[i+3+ln])<<16|uint32(b[i+4+ln])<<8|uint32(b[i+5+ln]) {
				c.RTCMBytes += int64(total)
				i += total
				continue
			}
		case b[i] == 0xB5 && i+1 < len(b) && b[i+1] == 0x62 && i+8 <= len(b):
			ln := int(b[i+4]) | int(b[i+5])<<8
			total := 8 + ln
			if i+total <= len(b) {
				var a, k byte
				for _, x := range b[i+2 : i+6+ln] {
					a += x
					k += a
				}
				if a == b[i+6+ln] && k == b[i+7+ln] {
					c.UBXBytes += int64(total)
					if b[i+2] == 0x02 && b[i+3] == 0x15 {
						c.RXMRawx++
					}
					if b[i+2] == 0x02 && b[i+3] == 0x13 {
						c.RXMSfrbx++
					}
					i += total
					continue
				}
			}
		}
		c.OtherBytes++
		i++
	}
	c.Total = c.UBXBytes + c.RTCMBytes + c.OtherBytes
	return c
}

var crc24qTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		v := uint32(i) << 16
		for j := 0; j < 8; j++ {
			v <<= 1
			if v&0x1000000 != 0 {
				v ^= 0x1864CFB
			}
		}
		t[i] = v & 0xFFFFFF
	}
	return t
}()

func crc24q(d []byte) uint32 {
	var crc uint32
	for _, x := range d {
		crc = ((crc << 8) & 0xFFFFFF) ^ crc24qTable[((crc>>16)^uint32(x))&0xFF]
	}
	return crc
}
