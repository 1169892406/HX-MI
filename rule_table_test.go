package main

import (
	"encoding/json"
	"testing"
)

func TestAllEmbeddedRulesParseAndMap(t *testing.T) {
	idx, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	if len(idx.services) < 20 || len(idx.sensors) < 1 || len(idx.degrade) < 1 {
		t.Fatalf("sizes %d/%d/%d", len(idx.services), len(idx.sensors), len(idx.degrade))
	}
	var sp miotSpec
	_ = json.Unmarshal([]byte(`{"services":[{"iid":2,"type":"urn:miot-spec-v2:service:light:1","properties":[{"iid":1,"type":"urn:miot-spec-v2:property:on:1","access":["read","write"]},{"iid":2,"type":"urn:miot-spec-v2:property:brightness:1","access":["read","write"],"unit":"percentage","value-range":[1,100,1]}]}]}`), &sp)
	b := idx.bind(sp)
	if len(b) != 1 || b[0].Capability != "light" || b[0].Properties["on"] != 1 || b[0].Properties["brightness"] != 2 {
		t.Fatalf("%+v", b)
	}
}
func TestRuleTableRejectsBadSchemaAndDuplicate(t *testing.T) {
	for _, raw := range []string{`{"version":2}`, `{"version":1,"services":[{"match":["x"],"capability":"a"},{"match":["x"],"capability":"b"}]}`, `{"version":1,"services":[{"match":["x"]}]}`} {
		if _, e := parseRuleIndex([]byte(raw)); e == nil {
			t.Fatal(raw)
		}
	}
}
func TestUnknownServiceFailClosed(t *testing.T) {
	idx, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	sp := miotSpec{Services: []miotService{{IID: 1, Type: "urn:miot-spec-v2:service:future-x:1"}}}
	if len(idx.bind(sp)) != 0 {
		t.Fatal("unknown mapped")
	}
}
