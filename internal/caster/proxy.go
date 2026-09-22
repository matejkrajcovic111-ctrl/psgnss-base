package caster

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// PROXY protocol support.
//
// Behind a TCP proxy every client appears to come from the proxy's address.
// This parses the HAProxy PROXY header (v1 text and v2 binary) so the caster
// can record the rover's real address instead.
//
// A header is only honoured from a trusted source address. Otherwise any client
// could forge its own origin, which would make the accounting worse than
// useless.

var v2Sig = []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// ProxyInfo is the real client address recovered from a PROXY header.
type ProxyInfo struct {
	SrcIP   string
	SrcPort int
	Used    bool
}

// TrustList decides which peers may send a PROXY header.
type TrustList struct{ nets []netip.Prefix }

func NewTrustList(cidrs []string) (*TrustList, error) {
	t := &TrustList{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// Accept a bare address as a single-host prefix.
		if !strings.Contains(c, "/") {
			a, err := netip.ParseAddr(c)
			if err != nil {
				return nil, fmt.Errorf("proxy_trusted %q: %w", c, err)
			}
			t.nets = append(t.nets, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("proxy_trusted %q: %w", c, err)
		}
		t.nets = append(t.nets, p)
	}
	return t, nil
}

func (t *TrustList) Trusted(addr net.Addr) bool {
	if t == nil || len(t.nets) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range t.nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ReadProxyHeader consumes a PROXY header if one is present.
//
// It only reads when the peer is trusted, so an untrusted client's request
// bytes are never mistaken for a header.
func ReadProxyHeader(br *bufio.Reader, peer net.Addr, trust *TrustList) (ProxyInfo, error) {
	if !trust.Trusted(peer) {
		return ProxyInfo{}, nil
	}
	head, err := br.Peek(12)
	if err != nil {
		if len(head) < 6 {
			return ProxyInfo{}, nil
		}
	}
	if len(head) >= 12 && string(head) == string(v2Sig) {
		return readV2(br)
	}
	if len(head) >= 6 && string(head[:6]) == "PROXY " {
		return readV1(br)
	}
	return ProxyInfo{}, nil
}

func readV1(br *bufio.Reader) (ProxyInfo, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return ProxyInfo{}, err
	}
	f := strings.Fields(strings.TrimRight(line, "\r\n"))
	// PROXY TCP4 srcIP dstIP srcPort dstPort
	if len(f) < 6 {
		if len(f) >= 2 && f[1] == "UNKNOWN" {
			return ProxyInfo{}, nil
		}
		return ProxyInfo{}, fmt.Errorf("malformed PROXY v1 header")
	}
	port, err := strconv.Atoi(f[4])
	if err != nil {
		return ProxyInfo{}, fmt.Errorf("malformed PROXY v1 source port %q", f[4])
	}
	if _, err := netip.ParseAddr(f[2]); err != nil {
		return ProxyInfo{}, fmt.Errorf("malformed PROXY v1 source address %q", f[2])
	}
	return ProxyInfo{SrcIP: f[2], SrcPort: port, Used: true}, nil
}

func readV2(br *bufio.Reader) (ProxyInfo, error) {
	hdr := make([]byte, 16)
	if _, err := ioReadFull(br, hdr); err != nil {
		return ProxyInfo{}, err
	}
	verCmd, fam := hdr[12], hdr[13]
	length := int(binary.BigEndian.Uint16(hdr[14:16]))
	body := make([]byte, length)
	if _, err := ioReadFull(br, body); err != nil {
		return ProxyInfo{}, err
	}
	if verCmd>>4 != 2 {
		return ProxyInfo{}, fmt.Errorf("unsupported PROXY version %d", verCmd>>4)
	}
	if verCmd&0x0F != 0x01 { // LOCAL, not PROXY: health check
		return ProxyInfo{}, nil
	}
	switch fam {
	case 0x11: // TCP over IPv4
		if len(body) < 12 {
			return ProxyInfo{}, fmt.Errorf("short PROXY v2 IPv4 body")
		}
		ip, _ := netip.AddrFromSlice(body[0:4])
		return ProxyInfo{SrcIP: ip.String(),
			SrcPort: int(binary.BigEndian.Uint16(body[8:10])), Used: true}, nil
	case 0x21: // TCP over IPv6
		if len(body) < 36 {
			return ProxyInfo{}, fmt.Errorf("short PROXY v2 IPv6 body")
		}
		ip, _ := netip.AddrFromSlice(body[0:16])
		return ProxyInfo{SrcIP: ip.String(),
			SrcPort: int(binary.BigEndian.Uint16(body[32:34])), Used: true}, nil
	}
	return ProxyInfo{}, nil
}

func ioReadFull(br *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := br.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
