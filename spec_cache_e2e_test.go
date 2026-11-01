package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

const specTestType = "urn:miot-spec-v2:device:light:0000A001:testcache:1"

func specTestBody() []byte {
	return []byte(`{"type":"` + specTestType + `","services":[{"iid":2,"type":"urn:miot-spec-v2:service:light:00007802:1","properties":[{"iid":1,"type":"urn:miot-spec-v2:property:on:00000006:1","format":"bool","access":["read","write"]}]}]}`)
}

func specCacheBytes(t *testing.T, typ string, fetched time.Time, sha string) []byte {
	t.Helper()
	b, err := json.Marshal(specCache{Version: 1, Model: typ, FetchedAt: fetched.UTC().Format(time.RFC3339), SHA256: sha, Spec: specTestBody()})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSpecCacheColdFetchThenWarm 验证首次抓取写缓存、二次命中缓存不打网络。
func TestSpecCacheColdFetchThenWarm(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	body := specTestBody()
	sum := hexSHA(body)

	// 冷启动：FS 里没有缓存，必须打网络并写缓存。
	h1 := hosttest.New(t)
	h1.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	var wrote []byte
	h1.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w struct {
			DataB64 string `json:"data_b64"`
		}
		_ = json.Unmarshal(raw, &w)
		wrote, _ = base64.StdEncoding.DecodeString(w.DataB64)
		return map[string]any{"ok": true}, nil
	})
	http1 := 0
	h1.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
		http1++
		return httpBody(body), nil
	})
	sp, err := loadMIoTSpec(ctx, specTestType)
	if err != nil || len(sp.Services) != 1 {
		t.Fatalf("cold fetch err=%v services=%d", err, len(sp.Services))
	}
	if http1 != 1 || len(wrote) == 0 {
		t.Fatalf("cold: http=%d wrote=%d", http1, len(wrote))
	}

	// 热启动：FS 返回有效缓存，不得打网络。
	h2 := hosttest.New(t)
	h2.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(specCacheBytes(t, specTestType, time.Now(), sum))})
	h2.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
		t.Fatal("valid cache hit should not call HTTP")
		return nil, nil
	})
	if sp, err := loadMIoTSpec(ctx, specTestType); err != nil || len(sp.Services) != 1 {
		t.Fatalf("warm err=%v", err)
	}
}

// TestSpecCacheRejectsCorruptAndExpired 验证摘要损坏与TTL过期都会重抓。
func TestSpecCacheRejectsCorruptAndExpired(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	sum := hexSHA(specTestBody())
	cases := []struct {
		name  string
		cache []byte
	}{
		{"corrupt-sha", specCacheBytes(t, specTestType, time.Now(), "deadbeef")},
		{"expired-ttl", specCacheBytes(t, specTestType, time.Now().Add(-8*24*time.Hour), sum)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := hosttest.New(t)
			h.Fixture(plugin.OpFSRead, map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(c.cache)})
			h.Fixture(plugin.OpFSWrite, map[string]any{"ok": true})
			n := 0
			h.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
				n++
				return httpBody(specTestBody()), nil
			})
			if _, err := loadMIoTSpec(ctx, specTestType); err != nil {
				t.Fatalf("refetch err=%v", err)
			}
			if n != 1 {
				t.Fatalf("expected refetch, http=%d", n)
			}
		})
	}
}

// TestSpecCacheNetworkFailureNoCacheFailClosed 验证无有效缓存时网络失败必须报错。
func TestSpecCacheNetworkFailureNoCacheFailClosed(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	h := hosttest.New(t)
	h.Fixture(plugin.OpFSRead, map[string]any{"found": false})
	h.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
		return nil, fmt.Errorf("HTTP denied")
	})
	if _, err := loadMIoTSpec(ctx, specTestType); err == nil {
		t.Fatal("network failure without cache must fail closed")
	}
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return fmt.Sprintf("%x", s[:]) }

// TestSpecCacheSingleflight 验证同一模型的并发加载只抓一次网络。
func TestSpecCacheSingleflight(t *testing.T) {
	ctx := &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh"}
	h := hosttest.New(t)
	var mu sync.Mutex
	files := map[string][]byte{}
	path := "spec-cache/" + safeModel(specTestType) + ".json"
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var r struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &r)
		mu.Lock()
		b, ok := files[r.Path]
		mu.Unlock()
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "data_b64": base64.StdEncoding.EncodeToString(b)}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var w struct {
			Path    string `json:"path"`
			DataB64 string `json:"data_b64"`
		}
		_ = json.Unmarshal(raw, &w)
		b, _ := base64.StdEncoding.DecodeString(w.DataB64)
		mu.Lock()
		files[w.Path] = b
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	})
	httpCount := 0
	h.On(plugin.OpHTTPRequest, func(json.RawMessage) (any, error) {
		mu.Lock()
		httpCount++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond) // 放大并发窗口
		return httpBody(specTestBody()), nil
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := loadMIoTSpec(ctx, specTestType)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("load err=%v", err)
		}
	}
	mu.Lock()
	got := httpCount
	mu.Unlock()
	if got != 1 {
		t.Fatalf("singleflight: http=%d want 1", got)
	}
	if _, ok := files[path]; !ok {
		t.Fatal("cache not written")
	}
}
