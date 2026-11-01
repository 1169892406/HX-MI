package main

import "testing"

func TestMapControlRuntimeEncoding(t *testing.T) {
	cmd := miioCommand{Action: "set_property", Params: map[string]any{"did": "d", "siid": 2, "piid": 3, "value": "#ff0000", "encode": "rgb_int", "meta": map[string]any{}}}
	m, p, e := mapControl(cmd)
	if e != nil || m != "set_properties" {
		t.Fatal(m, e)
	}
	list := p.([]map[string]any)
	if list[0]["value"] != 16711680 {
		t.Fatal(list)
	}
}
func TestExtractToggleBool(t *testing.T) {
	if v, ok := extractMIoTBool([]map[string]any{{"code": float64(0), "value": false}}); !ok || v {
		t.Fatal(v, ok)
	}
	if _, ok := extractMIoTBool([]map[string]any{{"code": float64(-1)}}); ok {
		t.Fatal()
	}
}
