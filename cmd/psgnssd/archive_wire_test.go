package main

import (
	"testing"

	"github.com/psgnss/psgnss-base/internal/hub"
)

func TestRTCMArchiveTakesMessageNumbers(t *testing.T) {
	if got, err := archiveMessageType(hub.ProtoRTCM3, "1020"); err != nil || got != 1020 {
		t.Fatalf("%d %v", got, err)
	}
	if _, err := archiveMessageType(hub.ProtoRTCM3, "RXM-SFRBX"); err == nil {
		t.Fatal("a UBX name was accepted for an RTCM archive")
	}
	if _, err := archiveMessageType(hub.ProtoUBX, "RXM-SFRBX"); err != nil {
		t.Fatal(err)
	}
}
