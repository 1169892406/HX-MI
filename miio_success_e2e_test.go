package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
	"testing"
	"time"
)

func decryptRequestID(t *testing.T, token, raw []byte) uint32 {
	env, e := miioDecryptPacket(token, raw, extractEncryptedID(token, raw))
	if e != nil {
		t.Fatal(e)
	}
	return uint32(env["id"].(float64))
}
func extractEncryptedID(token, raw []byte) uint32 {
	for id := uint32(40); id < 50; id++ {
		if _, e := miioDecryptPacket(token, raw, id); e == nil {
			return id
		}
	}
	return 0
}
func TestMIIOFakeSuccessClockAndResult(t *testing.T) {
	h := hosttest.New(t)
	token := []byte("0123456789abcdef")
	calls := 0
	fixed := time.Unix(1000, 0)
	h.On(plugin.OpUDPSend, func(req json.RawMessage) (any, error) {
		calls++
		var r plugin.UDPSendRequest
		_ = json.Unmarshal(req, &r)
		if calls == 1 {
			return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(helloReply(77, 900))}, nil
		}
		raw, _ := base64.StdEncoding.DecodeString(r.PayloadB64)
		if binary.BigEndian.Uint32(raw[8:12]) != 77 || binary.BigEndian.Uint32(raw[12:16]) != 900 {
			t.Fatalf("header dev=%d ts=%d", binary.BigEndian.Uint32(raw[8:12]), binary.BigEndian.Uint32(raw[12:16]))
		}
		resp, e := buildMIIOPacket(token, 77, 900, 42, "reply", map[string]any{"ok": true})
		if e != nil {
			t.Fatal(e)
		}
		return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(resp)}, nil
	})
	env, e := miioExchangeWith(&plugin.Context{}, miioDevice{Host: "192.0.2.2"}, token, "set_properties", nil, miioExchangeOptions{Now: func() time.Time { return fixed }, NextID: func() uint32 { return 42 }})
	if e != nil || env["params"] == nil {
		t.Fatalf("%v %v", env, e)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestMIIOTimeoutThenSecondSuccess(t *testing.T) {
	h := hosttest.New(t)
	token := []byte("0123456789abcdef")
	calls := 0
	h.On(plugin.OpUDPSend, func(req json.RawMessage) (any, error) {
		calls++
		if calls == 1 {
			return nil, &plugin.Error{Code: "timeout", Message: "timeout"}
		}
		if calls == 2 {
			return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(helloReply(1, 50))}, nil
		}
		resp, _ := buildMIIOPacket(token, 1, 50, 7, "reply", map[string]any{"ok": true})
		return map[string]any{"payload_b64": base64.StdEncoding.EncodeToString(resp)}, nil
	})
	_, e := miioExchangeWith(&plugin.Context{}, miioDevice{Host: "h"}, token, "x", nil, miioExchangeOptions{Now: func() time.Time { return time.Unix(100, 0) }, NextID: func() uint32 { return 7 }})
	if e != nil {
		t.Fatal(e)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
