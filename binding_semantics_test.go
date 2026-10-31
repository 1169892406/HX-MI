package main

import (
	"encoding/json"
	"testing"
)

func TestCompletePropertyActionEventBinding(t *testing.T) {
	idx, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	raw := `{"services":[{"iid":2,"type":"urn:miot-spec-v2:service:light:1","properties":[{"iid":1,"type":"urn:miot-spec-v2:property:on:1","format":"bool","access":["read","write"]},{"iid":2,"type":"urn:miot-spec-v2:property:brightness:1","format":"uint8","access":["read","write"],"unit":"percentage","value-range":[1,100,1],"value-list":[{"value":1,"description":"low"}]}],"actions":[{"iid":3,"type":"urn:miot-spec-v2:action:toggle:1","in":[1],"out":[2]}],"events":[{"iid":4,"type":"urn:miot-spec-v2:event:fault:1","arguments":[1,2]}]}]}`
	var sp miotSpec
	if json.Unmarshal([]byte(raw), &sp) != nil {
		t.Fatal()
	}
	bs := idx.bind(sp)
	if len(bs) != 1 {
		t.Fatal(bs)
	}
	b := bs[0]
	pm := b.PropertyMeta["brightness"]
	if !pm.Readable || !pm.Writable || pm.Format != "uint8" || pm.Unit != "percentage" || len(pm.Range) != 3 || pm.Values[1] != "low" {
		t.Fatalf("%+v", pm)
	}
	am := b.ActionMeta["toggle"]
	if am.AIID != 3 || len(am.Inputs) != 1 || len(am.Outputs) != 1 {
		t.Fatalf("%+v", am)
	}
	ev := b.Events["fault"]
	if ev.EIID != 4 || len(ev.Arguments) != 2 {
		t.Fatalf("%+v", ev)
	}
}
func TestReadOnlyFailClosed(t *testing.T) {
	idx, _ := parseRuleIndex(embeddedMappingRules)
	sp := miotSpec{Services: []miotService{{IID: 1, Type: "urn:miot-spec-v2:service:light:1", Properties: []miotProperty{{IID: 1, Type: "urn:miot-spec-v2:property:on:1", Access: []string{"read"}}}}}}
	b := idx.bind(sp)
	if len(b) != 1 || !b[0].ReadOnly {
		t.Fatalf("%+v", b)
	}
}
