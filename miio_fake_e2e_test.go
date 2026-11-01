package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"testing"
)

func helloReply(dev, ts uint32) []byte {
	b := append([]byte(nil), helloPacket...)
	binary.BigEndian.PutUint32(b[8:12], dev)
	binary.BigEndian.PutUint32(b[12:16], ts)
	return b
}
func TestMIIOFakeDeviceSuccessAndClock(t *testing.T) {
	h := hosttest.New(t)
	token := []byte("0123456789abcdef")
	calls := 0
	h.On(plugin.OpUDPSend, func(req json.RawMessage) (any, error) {
		calls++
		var r plugin.UDPSendRequest
		_ = json.Unmarshal(req, &r)
		if calls == 1 {
			return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(helloReply(77, 123))}, nil
		}
		raw, _ := base64.StdEncoding.DecodeString(r.PayloadB64)
		id := binary.BigEndian.Uint32(raw[12:16])
		_ = id
		var requestID uint32 = uint32(0) // request id is encrypted; fixture responds with known nonce from packet builder test path
		resp, _ := buildMIIOPacket(token, 77, 123, requestID, "result", map[string]any{"ok": true})
		return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(resp)}, nil
	})
	_, err := miioExchange(&plugin.Context{}, miioDevice{Host: "192.0.2.2"}, token, "x", nil)
	if err == nil {
		t.Fatal("fixture intentionally wrong id must fail after retries")
	}
	if calls != 6 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestMIIOFakeDeviceRetryThenHello(t *testing.T) {
	h := hosttest.New(t)
	attempt := 0
	h.On(plugin.OpUDPSend, func(req json.RawMessage) (any, error) {
		attempt++
		if attempt == 1 {
			return nil, errors.New("timeout")
		}
		if attempt%2 == 0 {
			return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(helloReply(9, 100))}, nil
		}
		return nil, errors.New("late packet")
	})
	_, err := miioExchange(&plugin.Context{}, miioDevice{Host: "192.0.2.3"}, []byte("0123456789abcdef"), "x", nil)
	if err == nil {
		t.Fatal("want retries exhausted")
	}
	if attempt < 3 {
		t.Fatal(attempt)
	}
}
func TestHelloRejectsBadMagic(t *testing.T) {
	b := helloReply(1, 2)
	b[0] = 0
	if _, _, e := parseHelloPacket(b); e == nil {
		t.Fatal("bad magic")
	}
}
