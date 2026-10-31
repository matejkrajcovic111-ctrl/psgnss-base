package unicore

import (
	"regexp"
	"strings"
)

// Class says how the console treats a command.
type Class int

const (
	// ClassQuery changes nothing: run it and show the answer.
	ClassQuery Class = iota
	// ClassSave writes the running configuration to flash (SAVECONFIG).
	ClassSave
	// ClassChange alters the running configuration: snapshot, apply, verify,
	// then hold pending Keep or auto-revert.
	ClassChange
	// ClassDangerous can take the station off the air; it needs an explicit
	// confirmation and is otherwise handled like a change when it can be
	// reverted.
	ClassDangerous
	// ClassRefused is never sent.
	ClassRefused
)

func (c Class) String() string {
	return [...]string{"query", "save", "change", "dangerous", "refused"}[c]
}

// Verdict is the classification of one console command.
type Verdict struct {
	Class Class
	// Revertable is false for commands whose effect the snapshot cannot undo
	// (a restart, a baud change on the port we are talking over); they are sent
	// directly once confirmed.
	Revertable bool
	// Reason explains Dangerous and Refused verdicts to the operator.
	Reason string
}

// firmwareWords are command names that would load or flash receiver firmware.
// The N4 command manual (R1.14) documents none: it says only that COM1 should
// be kept free for upgrading. The owner forbids flashing from this station, so
// the usual names for such commands are refused rather than guessed at.
var firmwareWords = map[string]bool{
	"UPGRADE": true, "UPDATE": true, "FWUPDATE": true, "FIRMWARE": true, "FWLOAD": true,
	"LOADFW": true, "UPFW": true, "LOADER": true, "BOOT": true, "BOOTLOADER": true,
	"BOOTMODE": true, "SETBOOT": true, "DFU": true, "DOWNLOAD": true, "IAP": true,
	"ISP": true, "FLASH": true, "ERASE": true, "PROGRAM": true, "BURN": true,
}

var portName = regexp.MustCompile(`^(COM[1-3]|ICOM\d+|NCOM\d+|ETH\d+|USB\d*|THISPORT|ALL)$`)

func isPort(s string) bool { return portName.MatchString(s) }

func refuse(why string) Verdict { return Verdict{Class: ClassRefused, Reason: why} }

func danger(revertable bool, why string) Verdict {
	return Verdict{Class: ClassDangerous, Revertable: revertable, Reason: why}
}

// Classify decides what the console may do with cmd. It reads only the first
// words, case-insensitively, as the receiver does.
func Classify(cmd string) Verdict {
	cmd = strings.TrimSpace(cmd)
	switch {
	case cmd == "":
		return refuse("Empty command.")
	case len(cmd) > 200:
		return refuse("Command is too long.")
	case strings.ContainsFunc(cmd, func(r rune) bool { return r < 0x20 || r > 0x7e }):
		return refuse("A command is one line of printable ASCII.")
	case cmd[0] == '$' || cmd[0] == '#':
		return refuse("Type the bare command; the console adds the line ending and the receiver adds the checksummed framing.")
	}
	t := strings.Fields(strings.ToUpper(cmd))
	head := t[0]
	if firmwareWords[head] {
		return refuse("Firmware upgrade and loader commands are refused: this station never flashes receiver firmware.")
	}
	switch head {
	case "FRESET":
		return danger(false, "FRESET erases the saved configuration (including the RTCM outputs and the base mode), the ephemerides and the stored position, resets the baud rate and restarts the receiver. The station goes off the air and must be reconfigured.")
	case "RESET":
		return danger(false, "RESET restarts the receiver (some forms also clear the configuration or ephemerides). The station is off the air while it restarts.")
	case "SAVECONFIG":
		return Verdict{Class: ClassSave}
	case "UNLOGALL":
		return danger(true, "UNLOGALL stops every output on every port, including all RTCM messages: the station goes off the air until the outputs are restored.")
	case "UNLOG":
		switch {
		case len(t) == 1:
			return danger(true, "UNLOG with no message stops every output on the port you are connected through, including all RTCM messages: the station goes off the air.")
		case len(t) == 2 && isPort(t[1]):
			return danger(true, "UNLOG "+t[1]+" with no message stops every output on that port, including all RTCM messages: the station goes off the air.")
		}
		return change()
	case "MODE":
		return classifyMode(t)
	case "CONFIG":
		return classifyConfig(t)
	case "MASK":
		if len(t) == 1 {
			return Verdict{Class: ClassQuery}
		}
		return change()
	case "UNMASK":
		return change()
	}
	// Anything else is a log request: "BESTNAVA" prints once, "BESTNAVA COM1"
	// prints once to that port, "RTCM1077 COM1 1" starts a periodic output.
	if len(t) == 1 || (len(t) == 2 && isPort(t[1])) {
		return Verdict{Class: ClassQuery}
	}
	return change()
}

var idNumber = regexp.MustCompile(`^[0-9]+$`)

func classifyMode(t []string) Verdict {
	if len(t) == 1 {
		return Verdict{Class: ClassQuery}
	}
	if t[1] != "BASE" {
		return danger(true, "MODE "+t[1]+" leaves base station mode: the receiver stops generating RTCM base-station messages and the station goes off the air.")
	}
	rest := t[2:]
	if len(rest) > 0 && idNumber.MatchString(rest[0]) && (len(rest) < 3 || rest[1] == "TIME" || len(rest) >= 4) {
		rest = rest[1:] // optional station ID
	}
	if len(rest) > 0 && rest[0] != "TIME" {
		return danger(true, "MODE BASE with a position fixes the base at those coordinates. If they are more than 300 m from the receiver's own solution it outputs no RTCM at all, and a wrong height or datum shifts every rover. Use Settings, Apply base position, which checks the distance.")
	}
	return change()
}

func classifyConfig(t []string) Verdict {
	if len(t) <= 2 {
		return Verdict{Class: ClassQuery}
	}
	switch t[1] {
	case "COM1":
		return danger(false, "COM1 is the port this station reads corrections from. Changing its baud rate or format disconnects the receiver until the station is restarted or the receiver power-cycled, and the change cannot be reverted over the link it breaks.")
	case "SIGNALGROUP":
		return danger(false, "CONFIG SIGNALGROUP resets the receiver and saves itself immediately (the manual says SAVECONFIG is unnecessary), so it cannot be auto-reverted. RTCM output stops while it restarts and the available signals change.")
	case "CMDFORMAT":
		return danger(false, "CONFIG CMDFORMAT 1 makes the receiver require an XOR checksum on every command; this console does not add one, so it would lock you out of the receiver.")
	}
	return change()
}

func change() Verdict { return Verdict{Class: ClassChange, Revertable: true} }
