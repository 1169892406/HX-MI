package main

import "testing"

func TestValueCodec(t *testing.T) {
	m := propertyBinding{Format: "uint8", Range: []float64{1, 100, 1}, Values: map[int]string{1: "low", 2: "high"}}
	if v, e := encodeProperty(m, "high", "enum_desc"); e != nil || v != 2 {
		t.Fatal(v, e)
	}
	if v, e := decodeProperty(m, 1); e != nil || v != "low" {
		t.Fatal(v, e)
	}
	if v, e := encodeProperty(propertyBinding{}, "#ff0000", "rgb_int"); e != nil || v != 16711680 {
		t.Fatal(v, e)
	}
	if _, e := encodeProperty(m, 101, ""); e == nil {
		t.Fatal("range")
	}
}
func TestReadonlyActionsFiltered(t *testing.T) {
	idx, _ := parseRuleIndex(embeddedMappingRules)
	sp := miotSpec{Services: []miotService{{IID: 1, Type: "urn:miot-spec-v2:service:light:1", Properties: []miotProperty{{IID: 1, Type: "urn:miot-spec-v2:property:on:1", Access: []string{"read"}}}}}}
	b := idx.bind(sp)
	if len(b) != 1 || !b[0].ReadOnly || len(b[0].Actions) != 0 {
		t.Fatalf("%+v", b)
	}
}
