package main

import "testing"

func TestEmbeddedCatalogModelIndexDeterministic(t *testing.T) {
	items, e := loadMIoTInstanceSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	a, e := BuildModelIndex(items)
	if e != nil {
		t.Fatal(e)
	}
	b, e := BuildModelIndex(items)
	if e != nil || a.hash != b.hash || len(a.exact) != 46203 {
		t.Fatalf("%d %s %s %v", len(a.exact), a.hash, b.hash, e)
	}
	for model := range a.exact {
		if x, ok := a.Resolve(model); !ok || x.Model != model {
			t.Fatal(model)
		}
		break
	}
}
func TestModelIndexVersionAndConflict(t *testing.T) {
	idx, e := BuildModelIndex([]MIoTInstance{{Model: "v.m", Type: "urn:miot-spec-v2:device:light:1", Version: 1, Status: "released"}, {Model: "v.m", Type: "urn:miot-spec-v2:device:light:2", Version: 2, Status: "preview"}})
	if e != nil {
		t.Fatal(e)
	}
	x, _ := idx.Resolve("v.m")
	if x.Version != 2 {
		t.Fatal(x)
	}
	if _, e = BuildModelIndex([]MIoTInstance{{Model: "x", Type: "a", Version: 1}, {Model: "x", Type: "b", Version: 1}}); e == nil {
		t.Fatal("conflict")
	}
}
func TestUnknownModelFailsClosed(t *testing.T) {
	idx, _ := BuildModelIndex([]MIoTInstance{{Model: "a.b", Type: "x", Version: 1}})
	if _, ok := idx.Resolve("unknown"); ok {
		t.Fatal()
	}
}
func TestCatalogCoverageReport(t *testing.T) {
	items, _ := loadMIoTInstanceSnapshot()
	idx, _ := BuildModelIndex(items)
	rules, e := parseRuleIndex(embeddedMappingRules)
	if e != nil {
		t.Fatal(e)
	}
	mapped, unmapped := idx.Coverage(rules)
	if mapped == 0 || len(unmapped) == 0 || mapped+len(unmapped) != len(idx.exact) {
		t.Fatalf("mapped=%d unmapped=%d total=%d", mapped, len(unmapped), len(idx.exact))
	}
}
