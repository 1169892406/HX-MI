package main

import (
	"encoding/binary"
	"testing"
)

func TestExternalByNumericID(t *testing.T) {
	p := make([]byte, 32)
	binary.BigEndian.PutUint32(p[8:12], 42)
	d, ok := externalByPacketDeviceID([]miioDevice{{ExternalID: "a", NumericDeviceID: 42}}, p)
	if !ok || d.ExternalID != "a" {
		t.Fatal(d, ok)
	}
	if _, ok = externalByPacketDeviceID(nil, p); ok {
		t.Fatal()
	}
}
func TestActivePacketDecryptAndChecksum(t *testing.T) {
	token := []byte("0123456789abcdef")
	pkt, e := buildMIIOPacket(token, 42, 1, 7, "x", map[string]any{})
	if e != nil {
		t.Fatal(e)
	}
	plain, e := decryptActivePacket(pkt, token)
	if e != nil || len(plain) == 0 {
		t.Fatal(string(plain), e)
	}
	pkt[16] ^= 1
	if _, e = decryptActivePacket(pkt, token); e == nil {
		t.Fatal("checksum")
	}
}
