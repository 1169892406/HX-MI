package main

import (
	"fmt"
	"testing"
)

// TestRC4BaselineCompare 用与旧版 cloud/rc4_baseline_test.go 相同的固定输入，
// 逐字节对比新实现的请求体构造。期望值来自旧版真实运行输出（黄金基线）。
func TestRC4BaselineCompare(t *testing.T) {
	ssecurity := "zXJmV29ya2VkRXhhbXBsZUtleQ=="
	nonce := "AAAAAAAAAAABaGVsbG93b3JsZA=="
	data := `{"getVirtualModel":true,"getHuamiDevices":1,"get_split_device":false,"support_smart_home":true}`
	form, signed, err := buildRC4Body("https://api.io.mi.com/app/home/device_list", ssecurity, data, nonce)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"__signed__":   "bhFBIYm07nXYEuoWjTgig7j4fe8NU7MtNZBU4kBom/U=",
		"data":         "0upzoIvgOVTP8sIBBLw/HDcXH8QmbRD2IhUNhWtHldQQg8tnoqjsbVk5m6YNmuD3CeLl3XLVGtlXj49qT8K3RktwNXhyYOEaeM4/GErsCVslCK0LRZ1LuHZBVh0lCsM=",
		"rc4_hash__":   "wLstlNTaGmTtxMcEA5EVOG0CasYzKi+AVz1YzA==",
		"signature":    "V/lyADW89AJU9aClsNiJQZweZJ4=",
		"ssecurity":    ssecurity,
		"_nonce":       nonce,
	}
	if signed != want["__signed__"] {
		t.Errorf("signed nonce 不一致:\n got %s\nwant %s", signed, want["__signed__"])
	}
	for k, v := range want {
		if k == "__signed__" {
			continue
		}
		if got := form.Get(k); got != v {
			t.Errorf("%s 不一致:\n got %s\nwant %s", k, got, v)
		}
	}
	fmt.Println("compare done")
}
