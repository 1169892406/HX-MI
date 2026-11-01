package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func encodeProperty(meta propertyBinding, v any, mode string) (any, error) {
	switch mode {
	case "rgb_int":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("hex string required")
		}
		s = strings.TrimPrefix(s, "#")
		n, e := strconv.ParseUint(s, 16, 24)
		if e != nil {
			return nil, e
		}
		return int(n), nil
	case "enum_desc":
		s := fmt.Sprint(v)
		for n, d := range meta.Values {
			if d == s {
				return n, nil
			}
		}
		return nil, errors.New("unknown enum")
	}
	switch meta.Format {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("bool required")
		}
		return b, nil
	case "uint8", "uint16", "uint32", "int8", "int16", "int32", "float":
		f, ok := toFloat(v)
		if !ok {
			return nil, errors.New("number required")
		}
		if len(meta.Range) >= 2 && (f < meta.Range[0] || f > meta.Range[1]) {
			return nil, errors.New("out of range")
		}
		if len(meta.Range) >= 3 && meta.Range[2] > 0 {
			f = math.Round((f-meta.Range[0])/meta.Range[2])*meta.Range[2] + meta.Range[0]
		}
		return f, nil
	case "string":
		return fmt.Sprint(v), nil
	}
	return v, nil
}
func decodeProperty(meta propertyBinding, v any) (any, error) {
	if len(meta.Values) > 0 {
		f, ok := toFloat(v)
		if !ok {
			return nil, errors.New("enum number required")
		}
		if s, ok := meta.Values[int(f)]; ok {
			return s, nil
		}
	}
	return v, nil
}
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case jsonNumber:
		f, e := strconv.ParseFloat(string(x), 64)
		return f, e == nil
	}
	return 0, false
}

type jsonNumber string
