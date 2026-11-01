package main

import (
	"testing"
)

func TestActionBatchContractIsSingleAction(t *testing.T) {
	in := []map[string]any{{"aiid": 1}, {"aiid": 2}, {"aiid": 3}}
	chunks := make([][]map[string]any, len(in))
	for i := range in {
		chunks[i] = []map[string]any{in[i]}
	}
	if len(chunks) != 3 {
		t.Fatal()
	}
	for i, c := range chunks {
		if len(c) != 1 || c[0]["aiid"] != in[i]["aiid"] {
			t.Fatal(i, c)
		}
	}
}
func TestPropertyBatchOrder(t *testing.T) {
	in := make([]map[string]any, 31)
	for i := range in {
		in[i] = map[string]any{"n": i}
	}
	chunks := chunk15(in)
	got := []int{}
	for _, c := range chunks {
		for _, m := range c {
			got = append(got, m["n"].(int))
		}
	}
	for i, n := range got {
		if i != n {
			t.Fatalf("%d %d", i, n)
		}
	}
}
