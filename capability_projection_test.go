package main

import (
	"reflect"
	"testing"
)

func TestCapabilitiesFromBindingsDedupSorted(t *testing.T) {
	bs := []capabilityBinding{
		{Capability: "switch"}, {Capability: "sensor"},
		{Capability: "switch"}, {Capability: ""}, {Capability: "climate"},
	}
	got := capabilitiesFromBindings(bs)
	want := []string{"climate", "sensor", "switch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestMergeCapabilitiesKeepsLegacyAndAddsProjected(t *testing.T) {
	got := mergeCapabilities([]string{"switch", "sensor"}, []string{"air_quality", "switch"})
	want := []string{"air_quality", "sensor", "switch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}
