package main

import "testing"

func TestActionParamsAndOutputs(t *testing.T) {
	c := miioCommand{ExternalID: "d", Action: "action", Params: map[string]any{"siid": 2, "aiid": 3, "in": []int{1, 2}, "inputs": []any{"hello", float64(7)}}}
	p, e := buildActionParams(c)
	if e != nil || p["aiid"] != 3 || len(p["in"].([]any)) != 2 {
		t.Fatal(p, e)
	}
	o, e := decodeActionOutputs(map[string]any{"out": []any{"ok", float64(8)}}, []int{4, 5})
	if e != nil || o["4"] != "ok" {
		t.Fatal(o, e)
	}
}
func TestActionUnknownParamsFailClosed(t *testing.T) {
	_, e := buildActionParams(miioCommand{Params: map[string]any{"siid": 2, "aiid": 3, "in": []int{1}, "inputs": []any{}}})
	if e == nil {
		t.Fatal()
	}
	if _, e = decodeActionOutputs(map[string]any{"out": []any{1}}, []int{1, 2}); e == nil {
		t.Fatal()
	}
}
func TestEventArgs(t *testing.T) {
	o, e := decodeEvent(eventBinding{EIID: 4, Arguments: []int{1, 2}}, []any{"x", true})
	if e != nil || o["2"] != true {
		t.Fatal(o, e)
	}
	if _, e = decodeEvent(eventBinding{Arguments: []int{1}}, nil); e == nil {
		t.Fatal()
	}
}
