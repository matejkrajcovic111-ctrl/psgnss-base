// Package unicoretest simulates a Unicore UM980's ASCII command interface
// closely enough to exercise PSGNSS's control path in tests. Its replies are
// byte-shaped like the live receiver's (firmware R4.10Build20739), because an
// idealised fake once hid three bugs at once: "$" lines checksum the '$' too,
// #MODE ends in a two-digit XOR that includes the '#', and #UNILOGLIST has no
// checksum and ends in a blank line.
package unicoretest

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is the receiver state shared by every pipe opened on it.
type Fake struct {
	mu sync.Mutex
	// Config maps a function name to its CONFIG line; Order keeps the listing
	// order stable.
	Config map[string]string
	Order  []string
	// Outputs maps "MSG PORT" to the rate text.
	Outputs map[string]string
	// ModeText is what the MODE query prints (before the trailing comma).
	ModeText string
	Masks    []string
	// Sent records every command received, in order.
	Sent []string
	// Saved is true once SAVECONFIG ran; SavedMode is ModeText at that time.
	Saved     bool
	SavedMode string
	// Reject lists command prefixes (upper case) the receiver refuses.
	Reject map[string]bool
	// Undulation is the geoid undulation of the (fake) site.
	Undulation float64
	// Silent drops every command without answering.
	Silent bool

	out  [][]byte
	wake chan struct{}
}

// New returns a receiver in the state the production UM980 was found in.
func New() *Fake {
	f := &Fake{Config: map[string]string{}, Outputs: map[string]string{},
		ModeText: "MODE BASE 1 TIME 60 0 0 1", Reject: map[string]bool{}, Undulation: 43,
		wake: make(chan struct{}, 1)}
	for _, l := range []string{"CONFIG ANTENNA POWERON", "CONFIG NMEAVERSION V410",
		"CONFIG PPS ENABLE GPS POSITIVE 500000 1000 0 0", "CONFIG COM1 115200", "CONFIG COM2 115200",
		"CONFIG SIGNALGROUP 2"} {
		name := strings.Fields(l)[1]
		f.Config[name] = l
		f.Order = append(f.Order, name)
	}
	for _, m := range []string{"RTCM1019", "RTCM1077", "RTCM1087", "RTCM1097"} {
		f.Outputs[m+" COM1"] = "1"
	}
	f.Outputs["RTCM1230 COM1"] = "10"
	f.Masks = []string{"MASK 5.000000"}
	return f
}

// Open returns a control pipe, matching the hub's Control signature.
func (f *Fake) Open() (io.ReadWriteCloser, error) { return pipe{f}, nil }

// Commands returns a copy of everything sent so far.
func (f *Fake) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Sent...)
}

// State returns the current MODE text and a sorted list of outputs.
func (f *Fake) State() (mode string, outputs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.Outputs {
		outputs = append(outputs, k+" "+v)
	}
	sort.Strings(outputs)
	return f.ModeText, outputs
}

type pipe struct{ f *Fake }

func (pipe) Close() error { return nil }

func (p pipe) Read(b []byte) (int, error) {
	f := p.f
	for i := 0; i < 2; i++ {
		f.mu.Lock()
		if len(f.out) > 0 {
			n := copy(b, f.out[0])
			if n < len(f.out[0]) {
				f.out[0] = f.out[0][n:]
			} else {
				f.out = f.out[1:]
			}
			f.mu.Unlock()
			return n, nil
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-time.After(20 * time.Millisecond):
		}
	}
	return 0, nil
}

func (p pipe) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		if strings.TrimSpace(line) != "" {
			p.f.handle(strings.TrimSpace(line))
		}
	}
	return len(b), nil
}

// checksum XORs the whole line up to the star, its leading '$' or '#' included.
func checksum(body string) string {
	var x byte
	for i := 0; i < len(body); i++ {
		x ^= body[i]
	}
	return fmt.Sprintf("%02X", x)
}

func (f *Fake) emit(s string) {
	f.out = append(f.out, []byte(s))
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Fake) ack(cmd, status string) {
	body := fmt.Sprintf("$command,%s,response: %s", cmd, status)
	f.emit(fmt.Sprintf("%s*%s\r\n", body, checksum(body)))
}

func (f *Fake) logOut(name, data string) {
	f.emit(fmt.Sprintf("#%s,95,GPS,FINE,2442,577931000,20739,0,18,510;%s*a34d9369\r\n", name, data))
}

func (f *Fake) handle(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Sent = append(f.Sent, line)
	if f.Silent {
		return
	}
	t := strings.Fields(strings.ToUpper(line))
	head := t[0]
	for p := range f.Reject {
		if strings.HasPrefix(strings.ToUpper(line), p) {
			f.ack(head, "PARSING FAILD NO MATCHING FUNC "+head)
			return
		}
	}
	switch head {
	case "VERSIONA":
		f.ack(head, "OK")
		f.logOut("VERSIONA", `"UM980","R4.10Build20739","HRPT00-S10C-P","2310415000001-MD22C5260607530","ff3a80e655b8f36c","2025/08/12"`)
	case "CONFIG":
		f.config(line, t)
	case "MODE":
		f.mode(line, t)
	case "MASK":
		f.mask(line, t)
	case "UNMASK":
		f.ack(head, "OK")
		want := "MASK " + strings.Join(t[1:], " ")
		kept := f.Masks[:0:0]
		for _, m := range f.Masks {
			if m != want {
				kept = append(kept, m)
			}
		}
		f.Masks = kept
	case "UNILOGLIST":
		f.ack(head, "OK")
		keys := make([]string, 0, len(f.Outputs))
		for k := range f.Outputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("#UNILOGLIST,95,GPS,FINE,2442,582500000,20739,0,18,510;")
		b.WriteString(fmt.Sprintf("\r\n<\t%d\r\n", len(keys)))
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("<\t%s %s \r\n", k, f.Outputs[k]))
		}
		b.WriteString("\r\n")
		f.emit(b.String())
	case "UNLOG":
		f.ack(head, "OK")
		switch len(t) {
		case 1:
			f.Outputs = map[string]string{}
		case 2:
			for k := range f.Outputs {
				if strings.HasSuffix(k, " "+t[1]) || strings.HasPrefix(k, t[1]+" ") {
					delete(f.Outputs, k)
				}
			}
		default:
			delete(f.Outputs, t[2]+" "+t[1])
		}
	case "SAVECONFIG":
		f.ack(head, "OK")
		f.Saved, f.SavedMode = true, f.ModeText
	case "FRESET", "RESET":
		f.ack(head, "OK")
	case "BESTNAVA":
		f.ack(head, "OK")
		f.logOut("BESTNAVA", fmt.Sprintf("SOL_COMPUTED,SINGLE,48.00000000000,17.00000000000,%.4f,%.4f,WGS84,1.2,1.1,2.1,\"0\",0.0,0.0,50,28,28,0,1,12,12,41", 300-f.Undulation, f.Undulation))
	case "BASEPOSA":
		f.ack(head, "OK")
		f.basepos()
	default:
		if len(t) >= 3 && (strings.HasPrefix(t[1], "COM") || strings.HasPrefix(t[1], "ICOM")) {
			f.ack(head, "OK") // "<LOG> <port> <rate>": enable
			f.Outputs[head+" "+t[1]] = strings.Join(t[2:], " ")
			return
		}
		f.ack(head, "OK") // one-shot log request
		f.logOut(head, "0")
	}
}

func (f *Fake) config(line string, t []string) {
	if len(t) == 1 {
		f.ack("CONFIG", "OK")
		for _, n := range f.Order {
			body := fmt.Sprintf("$CONFIG,%s,%s", n, f.Config[n])
			f.emit(fmt.Sprintf("%s*%s\r\n", body, checksum(body)))
		}
		return
	}
	f.ack("CONFIG", "OK")
	if len(t) > 2 {
		name := t[1]
		if _, ok := f.Config[name]; !ok {
			f.Order = append(f.Order, name)
		}
		f.Config[name] = "CONFIG " + strings.Join(strings.Fields(line)[1:], " ")
	}
}

func (f *Fake) mask(line string, t []string) {
	f.ack("MASK", "OK")
	if len(t) == 1 {
		for _, m := range f.Masks {
			body := "$CONFIG,MASK," + m
			f.emit(fmt.Sprintf("%s*%s\r\n", body, checksum(body)))
		}
		return
	}
	if _, err := strconv.ParseFloat(t[1], 64); err == nil {
		kept := f.Masks[:0:0]
		for _, m := range f.Masks {
			if _, e := strconv.ParseFloat(strings.TrimPrefix(m, "MASK "), 64); e != nil {
				kept = append(kept, m)
			}
		}
		f.Masks = append(kept, "MASK "+t[1])
		return
	}
	f.Masks = append(f.Masks, "MASK "+strings.Join(t[1:], " "))
}

func (f *Fake) mode(line string, t []string) {
	f.ack("MODE", "OK")
	if len(t) == 1 {
		body := fmt.Sprintf("#MODE,95,GPS,FINE,2442,582350000,20739,0,18,759;%s,", f.ModeText)
		f.emit(fmt.Sprintf("%s*%s\r\n", body, checksum(body)))
		return
	}
	f.ModeText = strings.Join(strings.Fields(line), " ")
}

// basepos answers as the live receiver does: the computed solution, whatever
// MODE BASE was given.
func (f *Fake) basepos() {
	f.logOut("BASEPOSA", fmt.Sprintf("SOL_COMPUTED,SBAS,48.00000000000,17.00000000000,%.4f,%.4f,WGS84,0.6,0.4,0.8,\"123\"", 300-f.Undulation, f.Undulation))
}
