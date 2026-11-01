package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"strings"
	"time"
)

var helloPacket = []byte{0x21, 0x31, 0, 0x20, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

type miioSession struct {
	DeviceID uint32
	DeltaTS  int64
	Seq      uint32
}

func parseHelloPacket(raw []byte) (uint32, uint32, error) {
	if len(raw) < 16 || raw[0] != 0x21 || raw[1] != 0x31 {
		return 0, 0, errors.New("miIO HELLO 应答非法")
	}
	return binary.BigEndian.Uint32(raw[8:12]), binary.BigEndian.Uint32(raw[12:16]), nil
}
func miioDecryptPacket(token, raw []byte, wantID uint32) (map[string]any, error) {
	if len(raw) < 48 || raw[0] != 0x21 || raw[1] != 0x31 {
		return nil, errors.New("miIO 应答形状非法")
	}
	body := raw[32:]
	h := md5.New()
	h.Write(raw[:16])
	h.Write(token)
	h.Write(body)
	if !bytes.Equal(h.Sum(nil), raw[16:32]) {
		return nil, errors.New("miIO checksum 不匹配")
	}
	key := md5.Sum(token)
	iv := md5.Sum(append(append([]byte{}, key[:]...), token...))
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv[:]).CryptBlocks(plain, body)
	n := int(plain[len(plain)-1])
	if n < 1 || n > 16 {
		return nil, errors.New("miIO padding 无效")
	}
	plain = bytes.TrimSuffix(plain[:len(plain)-n], []byte{0})
	var env map[string]any
	if json.Unmarshal(plain, &env) != nil {
		return nil, errors.New("miIO JSON 无效")
	}
	id, _ := env["id"].(float64)
	if uint32(id) != wantID {
		return nil, fmt.Errorf("miIO response id %d != request %d", uint32(id), wantID)
	}
	if e := env["error"]; e != nil {
		return nil, fmt.Errorf("miIO 设备错误: %v", e)
	}
	return env, nil
}

type miioExchangeOptions struct {
	Now      func() time.Time
	NextID   func() uint32
	Attempts int
}

func (o miioExchangeOptions) defaults() miioExchangeOptions {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NextID == nil {
		o.NextID = func() uint32 { return uint32(time.Now().UnixNano()) }
	}
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	return o
}
func miioExchange(ctx *plugin.Context, d miioDevice, token []byte, method string, params any) (map[string]any, error) {
	return miioExchangeWith(ctx, d, token, method, params, miioExchangeOptions{})
}
func miioExchangeWith(ctx *plugin.Context, d miioDevice, token []byte, method string, params any, opts miioExchangeOptions) (map[string]any, error) {
	opts = opts.defaults()
	var last error
	for attempt := 0; attempt < opts.Attempts; attempt++ {
		hello, err := ctx.UDPSend(plugin.UDPSendRequest{Host: d.Host, Port: 54321, PayloadB64: base64.StdEncoding.EncodeToString(helloPacket), TimeoutMS: 3000})
		if err != nil {
			if terminalResourceError(err) {
				return nil, err
			}
			last = err
			continue
		}
		hr, _ := base64.StdEncoding.DecodeString(hello.PayloadB64)
		dev, devTS, err := parseHelloPacket(hr)
		if err != nil {
			if terminalResourceError(err) {
				return nil, err
			}
			last = err
			continue
		}
		if d.NumericDeviceID != dev && d.ExternalID != "" {
			if e := persistNumericDeviceID(ctx, d.ExternalID, dev); e != nil {
				if !strings.Contains(e.Error(), "设备登记不存在") {
					return nil, fmt.Errorf("persist numeric device id: %w", e)
				}
			} else {
				d.NumericDeviceID = dev
			}
		}
		now := opts.Now()
		delta := now.Unix() - int64(devTS)
		id := opts.NextID()
		pkt, err := buildMIIOPacket(token, dev, uint32(now.Unix()-delta), id, method, params)
		if err != nil {
			return nil, err
		}
		resp, err := ctx.UDPSend(plugin.UDPSendRequest{Host: d.Host, Port: 54321, PayloadB64: base64.StdEncoding.EncodeToString(pkt), TimeoutMS: 3000})
		if err != nil {
			if terminalResourceError(err) {
				return nil, err
			}
			last = err
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(resp.PayloadB64)
		env, err := miioDecryptPacket(token, raw, id)
		if err != nil {
			if terminalResourceError(err) {
				return nil, err
			}
			last = err
			continue
		}
		return env, nil
	}
	return nil, fmt.Errorf("miIO %d次重试失败: %w", opts.Attempts, last)
}
func terminalResourceError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "PLUGIN_GENERATION_STALE") || strings.Contains(s, "PLUGIN_RESOURCE_EXHAUSTED") || strings.Contains(s, "PLUGIN_PERMISSION_DENIED")
}
func chunk15[T any](in []T) [][]T {
	out := [][]T{}
	for len(in) > 0 {
		n := 15
		if len(in) < n {
			n = len(in)
		}
		out = append(out, in[:n])
		in = in[n:]
	}
	return out
}
