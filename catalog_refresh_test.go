package main

import (
	"encoding/json"
	"testing"
)

func TestCoverageReportDeterministicAndKinds(t *testing.T) {
	items, _ := loadMIoTInstanceSnapshot()
	idx, _ := BuildModelIndex(items)
	rules, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	a := buildCoverageReport(idx, rules)
	b := buildCoverageReport(idx, rules)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) || a.TotalModels != 46203 || a.Mapped+a.Unmapped != a.TotalModels {
		t.Fatalf("%d/%d", a.Mapped, a.Unmapped)
	}
	if a.ByKind["services"] == 0 || a.ByKind["unmapped"] == 0 {
		t.Fatal(a.ByKind)
	}
	// 分类总数守恒（air_quality 只是独立桶，不并入 services）。
	if a.ByKind["services"]+a.ByKind["sensors"]+a.ByKind["air_quality"]+a.ByKind["degradations"]+a.ByKind["unmapped"] != a.TotalModels {
		t.Fatalf("kind 分类总数不等于 total: %v total=%d", a.ByKind, a.TotalModels)
	}
}
func TestCatalogCacheHashAndTTL(t *testing.T) {
	body := json.RawMessage(`{"instances":[{"model":"a","type":"t","status":"released","version":1}]}`)
	c := catalogCacheV2{Version: 2, FetchedAt: "2000-01-01T00:00:00Z", Body: body}
	if validCatalogCache(c) {
		t.Fatal("expired")
	}
}
func TestAliasConflictReport(t *testing.T) {
	idx, _ := BuildModelIndex([]MIoTInstance{{Model: "v1.same.x", Type: "t", Version: 1}, {Model: "v2.same.x", Type: "t", Version: 1}})
	r := buildCoverageReport(idx, &ruleIndex{services: map[string]serviceRule{}, sensors: map[string]sensorRule{}, degrade: map[string]degradeRule{}})
	if len(r.AliasConflicts) != 1 {
		t.Fatalf("%v", r.AliasConflicts)
	}
	if _, ok := idx.Resolve("same.x"); ok {
		t.Fatal("conflicted alias resolved")
	}
}

func TestCoverageAirQualityIndependent(t *testing.T) {
	idx, _ := BuildModelIndex([]MIoTInstance{{Model: "aq.one", Type: "urn:miot-spec-v2:service:air-quality:0000A001:aq:1", Status: "released", Version: 1}})
	rules := &ruleIndex{
		services:   map[string]serviceRule{"air-quality": {Capability: "air_quality"}},
		airQuality: map[string]serviceRule{"air-quality": {Capability: "air_quality"}},
		sensors:    map[string]sensorRule{}, degrade: map[string]degradeRule{},
	}
	r := buildCoverageReport(idx, rules)
	if r.ByKind["air_quality"] != 1 || r.ByKind["services"] != 0 || r.Mapped != 1 {
		t.Fatalf("air_quality 未独立统计/仍并入 services: %+v", r.ByKind)
	}
}
