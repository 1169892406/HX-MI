package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type ruleTable struct {
	Version        int           `json:"version"`
	Services       []serviceRule `json:"services"`
	SensorServices []sensorRule  `json:"sensor_services"`
	AirQuality     []serviceRule `json:"air_quality_services"`
	Degradations   []degradeRule `json:"degradations"`
}
type serviceRule struct {
	Match      []string              `json:"match"`
	Capability string                `json:"capability"`
	Props      map[string]string     `json:"props"`
	Actions    map[string]actionRule `json:"actions"`
}
type actionRule struct {
	Set       string `json:"set"`
	Toggle    string `json:"toggle"`
	Value     any    `json:"value"`
	FromParam string `json:"from_param"`
	Encode    string `json:"encode"`
}
type sensorRule struct {
	Match      []string                  `json:"match"`
	Capability string                    `json:"capability"`
	Props      map[string]sensorPropRule `json:"props"`
}
type sensorPropRule struct {
	Property    string `json:"prop"`
	DeviceClass string `json:"device_class"`
	Unit        string `json:"unit"`
}
type degradeRule struct {
	Service []string `json:"service"`
	To      []string `json:"to"`
	Reason  string   `json:"reason"`
	Mapping map[string]struct {
		Props   map[string]json.RawMessage `json:"props"`
		Actions map[string]actionRule      `json:"actions"`
	} `json:"mapping"`
}
type ruleIndex struct {
	table      ruleTable
	services   map[string]serviceRule
	airQuality map[string]serviceRule
	sensors    map[string]sensorRule
	degrade    map[string]degradeRule
}

// cachedRuleIndex 只解析一次规则表。
//
// 规则表是编译期嵌入的常量，每次命令都重新 JSON 解析 + 建 map 是纯浪费；
// -race 的宿主管道里这部分会把 60s 执行预算吃掉一大截（t205 实测）。
var (
	ruleOnce  sync.Once
	ruleCache *ruleIndex
	ruleErr   error
)

func cachedRuleIndex() (*ruleIndex, error) {
	ruleOnce.Do(func() {
		ruleCache, ruleErr = parseRuleIndex(embeddedMappingRules)
	})
	return ruleCache, ruleErr
}

func parseRuleIndex(raw []byte) (*ruleIndex, error) {
	var t ruleTable
	if json.Unmarshal(raw, &t) != nil || t.Version != 1 {
		return nil, errors.New("mapping schema/version invalid")
	}
	idx := &ruleIndex{table: t, services: map[string]serviceRule{}, airQuality: map[string]serviceRule{}, sensors: map[string]sensorRule{}, degrade: map[string]degradeRule{}}
	for _, r := range t.Services {
		if r.Capability == "" || len(r.Match) == 0 {
			return nil, errors.New("mapping service missing fields")
		}
		for _, m := range r.Match {
			m = normalizeName(m)
			if _, ok := idx.services[m]; ok {
				return nil, fmt.Errorf("duplicate service %s", m)
			}
			idx.services[m] = r
		}
	}
	// air_quality 独立索引：覆盖率不能把它算进 services（t187）。
	for _, r := range t.AirQuality {
		if r.Capability == "" || len(r.Match) == 0 {
			return nil, errors.New("mapping air_quality missing fields")
		}
		for _, m := range r.Match {
			m = normalizeName(m)
			if _, ok := idx.airQuality[m]; ok {
				return nil, fmt.Errorf("duplicate air_quality %s", m)
			}
			if _, ok := idx.services[m]; ok {
				return nil, fmt.Errorf("air_quality service overlaps services %s", m)
			}
			idx.airQuality[m] = r
		}
	}
	for _, r := range t.SensorServices {
		if r.Capability == "" {
			return nil, errors.New("sensor missing capability")
		}
		for _, m := range r.Match {
			m = normalizeName(m)
			if _, ok := idx.sensors[m]; ok {
				return nil, fmt.Errorf("duplicate sensor %s", m)
			}
			idx.sensors[m] = r
		}
	}
	for _, r := range t.Degradations {
		for _, m := range r.Service {
			m = normalizeName(m)
			if _, ok := idx.degrade[m]; ok {
				return nil, fmt.Errorf("duplicate degradation %s", m)
			}
			idx.degrade[m] = r
		}
	}
	return idx, nil
}
func normalizeName(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
func (idx *ruleIndex) bind(sp miotSpec) []capabilityBinding {
	out := []capabilityBinding{}
	for _, svc := range sp.Services {
		name := normalizeName(shortURN(svc.Type))
		if dg, ok := idx.degrade[name]; ok {
			out = append(out, bindDegradation(svc, dg)...)
			continue
		}
		r, ok := idx.services[name]
		if !ok {
			r, ok = idx.airQuality[name]
		}
		if !ok {
			if sr, sok := idx.sensors[name]; sok {
				for _, p := range svc.Properties {
					pn := normalizeName(shortURN(p.Type))
					if pr, yes := sr.Props[pn]; yes {
						b := capabilityBinding{Capability: sr.Capability, SIID: svc.IID, Properties: map[string]int{pr.Property: p.IID}, Actions: map[string]int{}, ReadOnly: true}
						out = append(out, b)
					}
				}
			}
			continue
		}
		b := capabilityBinding{Capability: r.Capability, SIID: svc.IID, Properties: map[string]int{}, Actions: map[string]int{}}
		writable := false
		for _, p := range svc.Properties {
			pn := normalizeName(shortURN(p.Type))
			if capProp, yes := r.Props[pn]; yes {
				b.Properties[capProp] = p.IID
				for _, a := range p.Access {
					writable = writable || a == "write"
				}
			}
		}
		enrichBinding(&b, svc, r)
		for an, ar := range r.Actions {
			propName := ar.Set
			if propName == "" {
				propName = ar.Toggle
			}
			if propName == "" {
				continue
			}
			capProp := r.Props[normalizeName(propName)]
			meta, ok := b.PropertyMeta[capProp]
			if !ok || !meta.Writable {
				delete(b.Actions, an)
				delete(b.ActionMeta, an)
			}
		}
		b.ReadOnly = !writable && len(b.Actions) == 0
		out = append(out, b)
	}
	return out
}
