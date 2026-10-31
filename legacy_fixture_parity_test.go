package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type fixtureSummary struct {
	Fixture      string   `json:"fixture"`
	Model        string   `json:"model"`
	Bindings     int      `json:"bindings"`
	Capabilities []string `json:"capabilities"`
	Properties   []string `json:"properties"`
	Actions      []string `json:"actions"`
	Events       []string `json:"events"`
}
type fixtureDiff struct {
	Fixture string `json:"fixture"`
	Field   string `json:"field"`
	Old     any    `json:"old"`
	New     any    `json:"new"`
}
type parityReport struct {
	Version     int                 `json:"version"`
	Fixtures    []fixtureSummary    `json:"fixtures"`
	Differences []fixtureDiff       `json:"differences"`
	Whitelist   []map[string]string `json:"whitelist"`
}

var legacyBaseline = map[string]fixtureSummary{
	"chuangmi.plug.m3.json":        {Bindings: 2, Capabilities: []string{"switch"}},
	"cnhdm.airrtc.wkq01.json":      {Bindings: 3, Capabilities: []string{"climate", "sensor", "switch"}},
	"xiaomi.aircondition.ma2.json": {Bindings: 2, Capabilities: []string{"climate", "sensor"}},
	"xiaomi.airp.mb5.json":         {Bindings: 6, Capabilities: []string{"fan", "sensor", "switch"}}}

func buildLegacyParity(t *testing.T) []byte {
	files, _ := filepath.Glob("legacy-fixtures/*.json")
	if len(files) != len(legacyBaseline) {
		t.Fatalf("fixtures=%d baseline=%d", len(files), len(legacyBaseline))
	}
	rules, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	r := parityReport{Version: 1, Fixtures: []fixtureSummary{}, Differences: []fixtureDiff{}, Whitelist: []map[string]string{}}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var sp miotSpec
		if json.Unmarshal(raw, &sp) != nil {
			t.Fatal(f)
		}
		bs := rules.bind(sp)
		s := fixtureSummary{Fixture: filepath.Base(f), Model: modelFromType(sp.Type), Bindings: len(bs), Capabilities: []string{}, Properties: []string{}, Actions: []string{}, Events: []string{}}
		sets := map[string]map[string]bool{"c": {}, "p": {}, "a": {}, "e": {}}
		for _, b := range bs {
			sets["c"][b.Capability] = true
			for k := range b.PropertyMeta {
				sets["p"][b.Capability+"."+k] = true
			}
			for k := range b.ActionMeta {
				sets["a"][b.Capability+"."+k] = true
			}
			for k := range b.Events {
				sets["e"][b.Capability+"."+k] = true
			}
		}
		s.Capabilities = sortedKeys(sets["c"])
		s.Properties = sortedKeys(sets["p"])
		s.Actions = sortedKeys(sets["a"])
		s.Events = sortedKeys(sets["e"])
		old := legacyBaseline[s.Fixture]
		if old.Bindings != s.Bindings {
			r.Differences = append(r.Differences, fixtureDiff{s.Fixture, "bindings", old.Bindings, s.Bindings})
		}
		if !equalStrings(old.Capabilities, s.Capabilities) {
			r.Differences = append(r.Differences, fixtureDiff{s.Fixture, "capabilities", old.Capabilities, s.Capabilities})
		}
		r.Fixtures = append(r.Fixtures, s)
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	return append(out, '\n')
}
func TestLegacyMIoTFixturesParity(t *testing.T) {
	a := buildLegacyParity(t)
	b := buildLegacyParity(t)
	if !bytes.Equal(a, b) {
		t.Fatal("report nondeterministic")
	}
	if e := os.WriteFile("legacy-fixture-diff.json", a, 0600); e != nil {
		t.Fatal(e)
	}
	var r parityReport
	_ = json.Unmarshal(a, &r)
	if len(r.Fixtures) != 4 {
		t.Fatal("empty report")
	}
	if len(r.Differences) == 0 {
		t.Fatal("empty differences gate: baseline comparison not exercised")
	}
	if len(r.Differences) != 4 {
		t.Fatalf("unexpected differences: %s", a)
	}
	approved := map[string]string{"cnhdm.airrtc.wkq01.json/capabilities": "空气质量属性升级为独立air_quality能力", "xiaomi.aircondition.ma2.json/capabilities": "空气质量属性升级为独立air_quality能力", "xiaomi.airp.mb5.json/bindings": "新映射合并同类sensor实例为air_quality", "xiaomi.airp.mb5.json/capabilities": "空气质量属性升级为独立air_quality能力"}
	for _, d := range r.Differences {
		reason, ok := approved[d.Fixture+"/"+d.Field]
		if !ok {
			t.Fatalf("unapproved difference: %+v", d)
		}
		r.Whitelist = append(r.Whitelist, map[string]string{"fixture": d.Fixture, "field": d.Field, "reason": reason})
	}
	final, _ := json.MarshalIndent(r, "", "  ")
	final = append(final, '\n')
	if e := os.WriteFile("legacy-fixture-diff.json", final, 0600); e != nil {
		t.Fatal(e)
	}
}
func sortedKeys(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func modelFromType(s string) string { return s }
