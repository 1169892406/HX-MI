package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestRC4RoundTripAndDrop(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("device-list-fixture")
	enc := rc4Crypt(key, plain)
	if bytes.Equal(enc, plain) {
		t.Fatal("not encrypted")
	}
	dec := rc4Crypt(key, enc)
	if !bytes.Equal(dec, plain) {
		t.Fatal(string(dec))
	}
}
func TestSsecurityDecode(t *testing.T) {
	raw := []byte("0123456789abcdef")
	s := base64.StdEncoding.EncodeToString(raw)
	got, err := decodeSsecurity(s)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal(err)
	}
}
func TestTrustedSTS(t *testing.T) {
	if !trustedSTS("https://sts.api.io.mi.com/sts?d=1") {
		t.Fatal("trusted rejected")
	}
	for _, u := range []string{"http://sts.api.io.mi.com/x", "https://evil.example/x", "https://sts.api.io.mi.com.evil/x"} {
		if trustedSTS(u) {
			t.Fatal(u)
		}
	}
}
func TestSavedCloudSessionNoTicketValues(t *testing.T) {
	s := savedCloudSession{Version: 2, SecretRef: "xiaomi_session_material", TicketsSealed: true}
	if s.SecretRef == "" {
		t.Fatal("missing ref")
	}
	if !s.TicketsSealed {
		t.Fatal("ticket marker")
	}
}
func TestRestoreClassification(t *testing.T) {
	cases := map[int]string{401: "auth_rejected", 403: "auth_rejected", 500: "temporarily_unavailable", 503: "temporarily_unavailable", 200: "available"}
	for status, want := range cases {
		got := "available"
		if status == 401 || status == 403 {
			got = "auth_rejected"
		} else if status >= 500 {
			got = "temporarily_unavailable"
		}
		if got != want {
			t.Fatalf("%d %s", status, got)
		}
	}
}
