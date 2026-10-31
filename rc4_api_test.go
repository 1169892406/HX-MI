package main

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNonceFixed12Bytes(t *testing.T) {
	n, e := genNonceAt(time.UnixMilli(60000 * 0x01020304))
	if e != nil {
		t.Fatal(e)
	}
	b, e := base64.StdEncoding.DecodeString(n)
	if e != nil || len(b) != 12 || string(b[8:]) != "\x01\x02\x03\x04" {
		t.Fatalf("%x %v", b, e)
	}
}
func TestRC4FormFakeServerDecryptVerify(t *testing.T) {
	ss := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	nonce := base64.StdEncoding.EncodeToString([]byte("12345678\x01\x02\x03\x04"))
	apiURL := "https://api.io.mi.com/app/home/device_list"
	form, signed, e := buildRC4Body(apiURL, ss, `{"x":1}`, nonce)
	if e != nil {
		t.Fatal(e)
	}
	data, e := decryptRC4Text(signed, form.Get("data"))
	if e != nil || string(data) != `{"x":1}` {
		t.Fatalf("%s %v", data, e)
	}
	hash, e := decryptRC4Text(signed, form.Get("rc4_hash__"))
	if e != nil {
		t.Fatal(e)
	}
	wantPre := rc4Sign("POST", apiURL, []orderedKV{{"data", `{"x":1}`}}, signed)
	if string(hash) != wantPre {
		t.Fatalf("%s != %s", hash, wantPre)
	}
	enc := []orderedKV{{"data", form.Get("data")}, {"rc4_hash__", form.Get("rc4_hash__")}}
	if form.Get("signature") != rc4Sign("POST", apiURL, enc, signed) {
		t.Fatal("bad post signature")
	}
	if form.Get("ssecurity") != ss || form.Get("_nonce") != nonce {
		t.Fatal("missing plain fields")
	}
}
func TestRC4BodyShape(t *testing.T) {
	ss := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12))
	f, _, e := buildRC4Body("https://api.io.mi.com/app/home/device_list", ss, "{}", nonce)
	if e != nil {
		t.Fatal(e)
	}
	encoded := f.Encode()
	v, _ := url.ParseQuery(encoded)
	for _, k := range []string{"data", "rc4_hash__", "signature", "ssecurity", "_nonce"} {
		if v.Get(k) == "" {
			t.Fatal(k)
		}
	}
	if strings.Contains(encoded, "%7B%7D") {
		t.Fatal("plain data leaked")
	}
}
