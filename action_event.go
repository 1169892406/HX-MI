package main

import (
	"errors"
	"fmt"
)

func buildActionParams(c miioCommand) (map[string]any, error) {
	p := copyParams(c.Params)
	aiid, ok := p["aiid"]
	if !ok {
		return nil, errors.New("action aiid缺失")
	}
	inputs, ok := p["inputs"].([]any)
	if !ok {
		if _, exists := p["inputs"]; exists {
			return nil, errors.New("action inputs形状非法")
		}
		inputs = []any{}
	}
	expected := toIntSlice(p["in"])
	if len(inputs) != len(expected) {
		return nil, fmt.Errorf("action inputs数量%d != %d", len(inputs), len(expected))
	}
	return map[string]any{"did": c.ExternalID, "siid": p["siid"], "aiid": aiid, "in": inputs}, nil
}
func decodeActionOutputs(row map[string]any, expected []int) (map[string]any, error) {
	raw, ok := row["out"].([]any)
	if !ok {
		if len(expected) == 0 {
			return map[string]any{}, nil
		}
		return nil, errors.New("action out缺失")
	}
	if len(raw) != len(expected) {
		return nil, errors.New("action out数量不匹配")
	}
	out := map[string]any{}
	for i, id := range expected {
		out[fmt.Sprint(id)] = raw[i]
	}
	return out, nil
}
func decodeEvent(binding eventBinding, args []any) (map[string]any, error) {
	if len(args) != len(binding.Arguments) {
		return nil, errors.New("event args数量不匹配")
	}
	out := map[string]any{}
	for i, id := range binding.Arguments {
		out[fmt.Sprint(id)] = args[i]
	}
	return out, nil
}
func firstRow(rows []map[string]any) map[string]any {
	if len(rows) == 0 {
		return map[string]any{}
	}
	return rows[0]
}
func toIntSlice(v any) []int {
	if x, ok := v.([]int); ok {
		return x
	}
	if xs, ok := v.([]any); ok {
		r := []int{}
		for _, v := range xs {
			switch n := v.(type) {
			case float64:
				r = append(r, int(n))
			case int:
				r = append(r, n)
			}
		}
		return r
	}
	return nil
}
