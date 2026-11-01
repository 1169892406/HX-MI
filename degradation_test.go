package main

import "testing"

func TestHumidifierDegradation(t *testing.T) {
	idx, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	sp := miotSpec{Services: []miotService{{IID: 2, Type: "urn:miot-spec-v2:service:humidifier:1", Properties: []miotProperty{{IID: 1, Type: "urn:miot-spec-v2:property:on:1", Access: []string{"read", "write"}}, {IID: 2, Type: "urn:miot-spec-v2:property:relative-humidity:1", Access: []string{"read"}}, {IID: 3, Type: "urn:miot-spec-v2:property:target-humidity:1", Access: []string{"read", "write"}}}}}}
	b := idx.bind(sp)
	if len(b) < 2 {
		t.Fatalf("%+v", b)
	}
	caps := map[string]bool{}
	for _, x := range b {
		caps[x.Capability] = true
	}
	if !caps["switch"] || !caps["sensor"] {
		t.Fatal(caps)
	}
}
