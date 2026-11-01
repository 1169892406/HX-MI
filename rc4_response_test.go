package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDecryptRC4RawCipherBytes(t *testing.T) {
	key := []byte("0123456789abcdef")
	signed := base64.StdEncoding.EncodeToString(key)
	plain := []byte(`{"result":{"list":[]}}`)
	cipher := rc4Crypt(key, plain)
	got, err := decryptRC4Bytes(signed, cipher)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("%s %v", got, err)
	}
}
func TestRawCipherIsNotBase64DecodedAgain(t *testing.T) {
	key := []byte("0123456789abcdef")
	signed := base64.StdEncoding.EncodeToString(key)
	cipher := rc4Crypt(key, []byte(`{"result":{}}`))
	if _, err := decryptRC4Text(signed, string(cipher)); err == nil {
		t.Fatal("raw cipher unexpectedly accepted as base64 text")
	}
}
func TestEmptyCipherRejected(t *testing.T) {
	signed := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	if _, err := decryptRC4Bytes(signed, nil); err == nil {
		t.Fatal("empty cipher accepted")
	}
}
