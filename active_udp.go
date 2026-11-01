package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"homex.dev/homex/sdk/go/plugin"
)

type udpDatagramEvent struct {
	DataB64 string `json:"data_b64"`
	Remote  string `json:"remote"`
}

func handleActiveUDP(ctx *plugin.Context, payload json.RawMessage) error {
	var ev udpDatagramEvent
	if json.Unmarshal(payload, &ev) != nil {
		return errors.New("udp event invalid")
	}
	pkt, e := base64.StdEncoding.DecodeString(ev.DataB64)
	if e != nil || len(pkt) < 32 {
		return errors.New("udp packet invalid")
	}
	ds, e := loadDeviceRegistry(ctx)
	if e != nil {
		return e
	}
	d, ok := externalByPacketDeviceID(ds, pkt)
	if !ok {
		return nil
	}
	token, _, e := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID)
	if e != nil || len(token) != 16 {
		return errors.New("mapped token unavailable")
	}
	plain, e := decryptActivePacket(pkt, token)
	if e != nil {
		return e
	}
	var env map[string]any
	if json.Unmarshal(plain, &env) != nil {
		return errors.New("active payload invalid")
	}
	env["external_id"] = d.ExternalID
	env["model"] = d.Model
	b, _ := json.Marshal(env)
	return handleMIoTEvent(ctx, b)
}
func decryptActivePacket(pkt, token []byte) ([]byte, error) {
	if len(pkt) < 48 || (len(pkt)-32)%16 != 0 {
		return nil, errors.New("packet length")
	}
	h := md5.New()
	h.Write(pkt[:16])
	h.Write(token)
	h.Write(pkt[32:])
	sum := h.Sum(nil)
	for i := range sum {
		if sum[i] != pkt[16+i] {
			return nil, errors.New("packet checksum")
		}
	}
	key := md5.Sum(token)
	x := append(append([]byte{}, key[:]...), token...)
	iv := md5.Sum(x)
	block, _ := aes.NewCipher(key[:])
	out := make([]byte, len(pkt)-32)
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(out, pkt[32:])
	n := int(out[len(out)-1])
	if n < 1 || n > 16 || n > len(out) {
		return nil, errors.New("padding")
	}
	return out[:len(out)-n], nil
}
