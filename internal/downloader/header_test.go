package downloader

import (
	"slices"
	"testing"
)

// convbin reads -hr as serial/type/version, the order of the RINEX
// REC # / TYPE / VERS line.
func TestReceiverHeaderCarriesSerialAndFirmware(t *testing.T) {
	d := &Downloader{o: Options{Receiver: "UNICORE UM980",
		ReceiverFirmware: "R4.10Build20739", ReceiverSerial: "MD22C5260607530"}}
	args := d.headerArgs()
	i := slices.Index(args, "-hr")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -hr in %v", args)
	}
	if got, want := args[i+1], "MD22C5260607530/UNICORE UM980/R4.10Build20739"; got != want {
		t.Errorf("-hr = %q, want %q", got, want)
	}

	d.o.ReceiverFirmware, d.o.ReceiverSerial = "", ""
	args = d.headerArgs()
	if got, want := args[slices.Index(args, "-hr")+1], "0000/UNICORE UM980/"; got != want {
		t.Errorf("-hr without serial or firmware = %q, want %q", got, want)
	}
}
