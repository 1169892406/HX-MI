package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

type miioDevice struct {
	ExternalID      string   `json:"external_id"`
	Host            string   `json:"host"`
	TokenRef        string   `json:"token_ref,omitempty"`
	Model           string   `json:"model"`
	Name            string   `json:"name"`
	Capabilities    []string `json:"capabilities"`
	NumericDeviceID uint32   `json:"numeric_device_id,omitempty"`
}
type miioCommand struct {
	ExternalID string         `json:"external_id"`
	Action     string         `json:"action"`
	Capability string         `json:"capability"`
	// Params 是插件内部归一化后的参数袋。
	Params map[string]any `json:"params"`
	// Args 是宿主 pluginres.CommandRequest 的线上字段（resource_host.go 写
	// "args"）。早期插件只认 "params"，导致 set_property/set-brightness 这类
	// 带参命令全部拿到空参数（t234 实测：Binding 不支持 action=set_property）。
	// 二者都收，优先 params（内部/测试），缺省回落到 args（宿主真实下发）。
	Args map[string]any `json:"args"`
}

func miioPKCS7(b []byte) []byte {
	n := aes.BlockSize - len(b)%aes.BlockSize
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}
func buildMIIOPacket(token []byte, deviceID, ts, id uint32, method string, params any) ([]byte, error) {
	if len(token) != 16 {
		return nil, errors.New("miIO token 必须16字节")
	}
	plain, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	plain = append(plain, 0)
	key := md5.Sum(token)
	iv := md5.Sum(append(append([]byte{}, key[:]...), token...))
	block, _ := aes.NewCipher(key[:])
	p := miioPKCS7(plain)
	enc := make([]byte, len(p))
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(enc, p)
	raw := make([]byte, 32, 32+len(enc))
	raw[0] = 0x21
	raw[1] = 0x31
	binary.BigEndian.PutUint16(raw[2:4], uint16(32+len(enc)))
	binary.BigEndian.PutUint32(raw[8:12], deviceID)
	binary.BigEndian.PutUint32(raw[12:16], ts)
	h := md5.New()
	h.Write(raw[:16])
	h.Write(token)
	h.Write(enc)
	copy(raw[16:32], h.Sum(nil))
	return append(raw, enc...), nil
}
func handleDriverCommand(ctx *plugin.Context, payload json.RawMessage) error {
	var cmd miioCommand
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return err
	}
	if len(cmd.Params) == 0 && len(cmd.Args) > 0 {
		cmd.Params = cmd.Args
	}
	devices, err := loadDeviceRegistry(ctx)
	if err != nil {
		return err
	}
	var d *miioDevice
	for i := range devices {
		if devices[i].ExternalID == cmd.ExternalID {
			d = &devices[i]
			break
		}
	}
	if d == nil {
		return errors.New("设备未登记")
	}
	cmd, err = resolveCommandBinding(ctx, *d, cmd)
	if err != nil {
		return err
	}
	token, _, err := ctx.VaultBlobOpenDevice("miio_token", d.ExternalID)
	if err != nil || len(token) != 16 {
		return errors.New("设备 token Vault 不可用")
	}
	method, params, err := mapControl(cmd)
	if err != nil {
		return err
	}
	if strings.EqualFold(cmd.Action, "toggle") {
		siid, piid := 2, 1
		if v, ok := cmd.Params["siid"].(float64); ok {
			siid = int(v)
		}
		if v, ok := cmd.Params["piid"].(float64); ok {
			piid = int(v)
		}
		res, e := miioBatchProperties(ctx, *d, token, "get_properties", []map[string]any{{"did": d.ExternalID, "siid": siid, "piid": piid}})
		if e != nil {
			return e
		}
		current, ok := extractMIoTBool(res)
		if !ok {
			return errors.New("toggle 当前值不可用")
		}
		method = "set_properties"
		params = []map[string]any{{"did": d.ExternalID, "siid": siid, "piid": piid, "value": !current}}
	}
	state := map[string]any{"last_command": cmd.Action, "available": true}
	if list, ok := params.([]map[string]any); ok && (method == "set_properties" || method == "get_properties" || method == "action") {
		rows, e := miioBatchProperties(ctx, *d, token, method, list)
		if e != nil {
			return e
		}
		if method == "action" {
			decoded, e := decodeActionOutputs(firstRow(rows), toIntSlice(cmd.Params["out"]))
			if e != nil {
				return e
			}
			state["action_out"] = decoded
		}
	} else {
		if _, err = miioExchange(ctx, *d, token, method, params); err != nil {
			return err
		}
	}
	return ctx.DeviceStatePublish(plugin.DeviceStateRequest{ExternalID: d.ExternalID, State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
func mapControl(c miioCommand) (string, any, error) {
	switch strings.ToLower(c.Action) {
	case "turn_on":
		return "set_properties", []map[string]any{{"did": c.ExternalID, "siid": 2, "piid": 1, "value": true}}, nil
	case "turn_off":
		return "set_properties", []map[string]any{{"did": c.ExternalID, "siid": 2, "piid": 1, "value": false}}, nil
	case "toggle":
		return "toggle", c.Params, nil
	case "set_property":
		p := map[string]any{}
		for k, v := range c.Params {
			p[k] = v
		}
		if metaRaw, ok := p["meta"].(map[string]any); ok {
			meta := propertyBinding{}
			b, _ := json.Marshal(metaRaw)
			_ = json.Unmarshal(b, &meta)
			encoded, e := encodeProperty(meta, p["value"], fmt.Sprint(p["encode"]))
			if e != nil {
				return "", nil, e
			}
			p["value"] = encoded
			delete(p, "meta")
			delete(p, "encode")
		}
		return "set_properties", []map[string]any{p}, nil
	case "action":
		p, e := buildActionParams(c)
		if e != nil {
			return "", nil, e
		}
		return "action", []map[string]any{p}, nil
	default:
		return "", nil, fmt.Errorf("不支持动作 %s", c.Action)
	}
}
func base64String(b []byte) string { return encodeBase64(b) }
func encodeBase64(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		remain := len(b) - i
		v = uint32(b[i]) << 16
		if remain > 1 {
			v |= uint32(b[i+1]) << 8
		}
		if remain > 2 {
			v |= uint32(b[i+2])
		}
		out.WriteByte(table[(v>>18)&63])
		out.WriteByte(table[(v>>12)&63])
		if remain > 1 {
			out.WriteByte(table[(v>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if remain > 2 {
			out.WriteByte(table[v&63])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}
