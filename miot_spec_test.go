package main

import (
	"encoding/json"
	"testing"
)

func TestEmbeddedRulesAndBinding(t *testing.T) {
	if e := validateEmbeddedRules(); e != nil {
		t.Fatal(e)
	}
	raw := `{"type":"x","services":[{"iid":2,"type":"urn:miot-spec-v2:service:light:00007802:1","properties":[{"iid":1,"type":"urn:miot-spec-v2:property:on:00000006:1","access":["read","write"]}],"actions":[{"iid":1,"type":"urn:miot-spec-v2:action:toggle:0000280c:1"}]}]}`
	var sp miotSpec
	if json.Unmarshal([]byte(raw), &sp) != nil {
		t.Fatal()
	}
	b := mapSpec(sp)
	if len(b) != 1 || b[0].Capability != "light" || b[0].Properties["on"] != 1 || b[0].Actions["toggle"] != 1 || b[0].ReadOnly {
		t.Fatalf("%+v", b)
	}
}
func TestUnknownSpecFailClosed(t *testing.T) {
	sp := miotSpec{Services: []miotService{{IID: 9, Type: "urn:miot-spec-v2:service:unknown:1", Properties: []miotProperty{{IID: 1}}}}}
	if len(mapSpec(sp)) != 0 {
		t.Fatal("unknown mapped")
	}
}
func TestSafeModelStable(t *testing.T) {
	if safeModel("a") != safeModel("a") || safeModel("a") == safeModel("b") {
		t.Fatal()
	}
}
