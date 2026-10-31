package main

import "encoding/json"

func bindDegradation(svc miotService, d degradeRule) []capabilityBinding {
	out := []capabilityBinding{}
	for _, cap := range d.To {
		m, ok := d.Mapping[cap]
		if !ok {
			continue
		}
		b := capabilityBinding{Capability: cap, SIID: svc.IID, Properties: map[string]int{}, PropertyMeta: map[string]propertyBinding{}, Actions: map[string]int{}, ActionMeta: map[string]actionBinding{}, Events: map[string]eventBinding{}, ReadOnly: true}
		for src, raw := range m.Props {
			dst := src
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				dst = s
			} else {
				var o struct {
					Prop        string   `json:"prop"`
					DeviceClass string   `json:"device_class"`
					Unit        string   `json:"unit"`
					Min         *float64 `json:"min"`
					Max         *float64 `json:"max"`
					Step        *float64 `json:"step"`
				}
				if json.Unmarshal(raw, &o) == nil && o.Prop != "" {
					dst = o.Prop
				}
				for _, p := range svc.Properties {
					if normalizeName(shortURN(p.Type)) == normalizeName(src) {
						meta := propertyMeta(p)
						if o.Unit != "" {
							meta.Unit = o.Unit
						}
						if o.DeviceClass != "" {
							meta.DeviceClass = o.DeviceClass
						}
						if o.Min != nil && o.Max != nil {
							step := 1.0
							if o.Step != nil {
								step = *o.Step
							}
							meta.Range = []float64{*o.Min, *o.Max, step}
						}
						b.Properties[dst] = p.IID
						b.PropertyMeta[dst] = meta
						if meta.Writable {
							b.ReadOnly = false
						}
					}
				}
				continue
			}
			for _, p := range svc.Properties {
				if normalizeName(shortURN(p.Type)) == normalizeName(src) {
					b.Properties[dst] = p.IID
					b.PropertyMeta[dst] = propertyMeta(p)
					if b.PropertyMeta[dst].Writable {
						b.ReadOnly = false
					}
				}
			}
		}
		for an, ar := range m.Actions {
			prop := ar.Set
			if prop == "" {
				prop = ar.Toggle
			}
			for dst, meta := range b.PropertyMeta {
				if normalizeName(dst) == normalizeName(prop) && meta.Writable {
					b.Actions[an] = 0
					b.ActionMeta[an] = actionBinding{Rule: ar}
					b.ReadOnly = false
				}
			}
		}
		if len(b.Properties) > 0 || len(b.Actions) > 0 {
			out = append(out, b)
		}
	}
	return out
}
