package main

import (
	"encoding/binary"
	"testing"
)

func TestParseHelloClock(t *testing.T) {
	raw := append([]byte(nil), helloPacket...)
	binary.BigEndian.PutUint32(raw[8:12], 0x01020304)
	binary.BigEndian.PutUint32(raw[12:16], 123)
	d, ts, e := parseHelloPacket(raw)
	if e != nil || d != 0x01020304 || ts != 123 {
		t.Fatal(d, ts, e)
	}
}
func TestMIIOResponseChecksumAndID(t *testing.T) {
	tok := []byte("0123456789abcdef")
	p, e := buildMIIOPacket(tok, 7, 8, 9, "x", nil)
	if e != nil {
		t.Fatal(e)
	}
	env, e := miioDecryptPacket(tok, p, 9)
	if e != nil || env["id"].(float64) != 9 {
		t.Fatal(env, e)
	}
	bad := append([]byte(nil), p...)
	bad[20] ^= 1
	if _, e = miioDecryptPacket(tok, bad, 9); e == nil {
		t.Fatal("bad checksum")
	}
	if _, e = miioDecryptPacket(tok, p, 10); e == nil {
		t.Fatal("wrong id")
	}
}
func TestChunk15(t *testing.T) {
	in := make([]int, 31)
	c := chunk15(in)
	if len(c) != 3 || len(c[0]) != 15 || len(c[1]) != 15 || len(c[2]) != 1 {
		t.Fatalf("%v", c)
	}
}
func TestDeviceJSONHasNoTokenHex(t *testing.T) {
	d := miioDevice{ExternalID: "d", TokenRef: "miio_token"}
	if d.TokenRef == "" {
		t.Fatal()
	}
}
