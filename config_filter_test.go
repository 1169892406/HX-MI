package main

import "testing"

func TestModelAllowedFilter(t *testing.T) {
	saved := current
	defer func() { current = saved }()
	current.DeviceFilterModels = nil
	if !modelAllowed("anything") {
		t.Fatal("空过滤应允许全部")
	}
	current.DeviceFilterModels = []string{"chuangmi.", "lumi."}
	if !modelAllowed("chuangmi.plug.m3") || !modelAllowed("lumi.sensor_gas.mcn02") {
		t.Fatal("前缀匹配应允许")
	}
	if modelAllowed("xiaomi.aircondition.ma2") {
		t.Fatal("不匹配应拒绝")
	}
}
