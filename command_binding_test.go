package main

import "testing"

func TestInjectCommandBinding(t *testing.T) {
	bs := []capabilityBinding{{Capability: "light", SIID: 2, PropertyMeta: map[string]propertyBinding{"on": {PIID: 1, Writable: true, Format: "bool"}}, ActionMeta: map[string]actionBinding{}}}
	c, e := injectCommand(miioCommand{Action: "turn_on", Capability: "light", Params: map[string]any{}}, bs)
	if e != nil {
		t.Fatal(e)
	}
	if c.Params["siid"] != 2 || c.Params["piid"] != 1 {
		t.Fatal(c.Params)
	}
	if _, ok := c.Params["meta"].(propertyBinding); !ok {
		t.Fatal(c.Params)
	}
}
func TestInjectUnknownFailsClosed(t *testing.T) {
	_, e := injectCommand(miioCommand{Action: "turn_on", Capability: "light", Params: map[string]any{}}, nil)
	if e == nil {
		t.Fatal()
	}
}
func TestInjectActionAIID(t *testing.T) {
	bs := []capabilityBinding{{Capability: "speaker", SIID: 4, ActionMeta: map[string]actionBinding{"play": {AIID: 3, Inputs: []int{1}}}}}
	c, e := injectCommand(miioCommand{Action: "play", Capability: "speaker", Params: map[string]any{}}, bs)
	if e != nil || c.Params["aiid"] != 3 {
		t.Fatal(c, e)
	}
}
