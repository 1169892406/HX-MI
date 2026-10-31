package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestMIIOPacketShapeAndNoPlaintext(t *testing.T) {
	tok, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	p, err := buildMIIOPacket(tok, 0x01020304, 0x05060708, 7, "set_properties", []map[string]any{{"siid": 2, "piid": 1, "value": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p) < 48 || p[0] != 0x21 || p[1] != 0x31 {
		t.Fatalf("%x", p[:4])
	}
	if bytes.Contains(p, []byte("set_properties")) {
		t.Fatal("plaintext leaked")
	}
}
func TestMapControl(t *testing.T) {
	m, p, e := mapControl(miioCommand{ExternalID: "d", Action: "turn_on"})
	if e != nil || m != "set_properties" || len(p.([]map[string]any)) != 1 {
		t.Fatal(m, p, e)
	}
	if _, _, e = mapControl(miioCommand{Action: "bad"}); e == nil {
		t.Fatal("want error")
	}
}
func TestMIIORejectsBadToken(t *testing.T) {
	if _, e := buildMIIOPacket([]byte("short"), 1, 1, 1, "x", nil); e == nil {
		t.Fatal("want error")
	}
}
func TestBase64Fixture(t *testing.T) {
	if got := base64String([]byte{1, 2, 3}); got != "AQID" {
		t.Fatal(got)
	}
}
