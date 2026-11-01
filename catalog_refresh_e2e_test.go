package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"homex.dev/homex/sdk/go/plugin"
	"homex.dev/homex/sdk/go/plugin/hosttest"
)

type catalogFixture struct {
	t        *testing.T
	files    map[string][]byte
	mu       sync.Mutex
	requests []plugin.HTTPRequest
	http     atomic.Int64
	respond  func(plugin.HTTPRequest) (plugin.HTTPResponse, error)
	release  chan struct{}
	block    atomic.Bool
}

func newCatalogFixture(t *testing.T) (*plugin.Context, *catalogFixture) {
	t.Helper()
	resetCatalogRefreshState()
	h := hosttest.New(t)
	f := &catalogFixture{t: t, files: map[string][]byte{}}
	h.On(plugin.OpFSRead, func(raw json.RawMessage) (any, error) {
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &in)
		f.mu.Lock()
		b, ok := f.files[in.Path]
		f.mu.Unlock()
		if !ok {
			return plugin.FSReadResponse{Found: false}, nil
		}
		return plugin.FSReadResponse{Found: true, DataB64: base64.StdEncoding.EncodeToString(b)}, nil
	})
	h.On(plugin.OpFSWrite, func(raw json.RawMessage) (any, error) {
		var req plugin.FSWriteRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(req.DataB64)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.files[req.Path] = append([]byte(nil), data...)
		f.mu.Unlock()
		return map[string]any{"ok": true}, nil
	})
	h.On(plugin.OpHTTPRequest, func(raw json.RawMessage) (any, error) {
		var req plugin.HTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		n := f.http.Add(1)
		_ = n
		if f.block.Load() {
			<-f.release
		}
		if f.respond == nil {
			return nil, errors.New("no HTTP fixture")
		}
		return f.respond(req)
	})
	return &plugin.Context{PluginID: "homex.xiaomi_miot", HouseholdID: "hh-test"}, f
}

func resetCatalogRefreshState() {
	catalogRefreshMu.Lock()
	catalogRefreshing = false
	catalogLastRefresh = time.Time{}
	catalogRefreshCount = 0
	catalogRefreshFailures = 0
	catalogRefreshCond.Broadcast()
	catalogRefreshMu.Unlock()
}

func mustSHAHex(b []byte) string {
	s := sha256.Sum256(b)
	return fmt.Sprintf("%x", s[:])
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func rawCatalogBody(t *testing.T, models ...string) []byte {
	t.Helper()
	items := make([]MIoTInstance, 0, len(models))
	for _, m := range models {
		items = append(items, MIoTInstance{Model: m, Type: "urn:miot-spec-v2:device:light:0000A001:" + m, Status: "released", Version: 1})
	}
	b, err := json.Marshal(miotInstanceSnapshot{Instances: items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func httpBody(b []byte) plugin.HTTPResponse {
	return plugin.HTTPResponse{Status: 200, Headers: map[string]string{"ETag": `"v1"`}, BodyB64: base64.StdEncoding.EncodeToString(b)}
}

func TestCatalogRefreshHosttest200CacheETag304AndDualHash(t *testing.T) {
	ctx, f := newCatalogFixture(t)
	body := rawCatalogBody(t, "a.light", "b.light")
	f.respond = func(req plugin.HTTPRequest) (plugin.HTTPResponse, error) {
		if req.TimeoutMS != 20000 {
			t.Errorf("HTTP timeout=%d, want 20000", req.TimeoutMS)
		}
		return httpBody(body), nil
	}
	idx, source, err := refreshModelIndex(ctx, false)
	if err != nil || source != "network" || len(idx.exact) != 2 {
		t.Fatalf("network refresh=%v %q %v", idx, source, err)
	}
	if f.http.Load() != 1 || len(f.files["spec-cache/instances-v2.json"]) == 0 {
		t.Fatalf("HTTP=%d cache=%d", f.http.Load(), len(f.files["spec-cache/instances-v2.json"]))
	}
	var cache catalogCacheV2
	if err := json.Unmarshal(f.files["spec-cache/instances-v2.json"], &cache); err != nil {
		t.Fatal(err)
	}
	if !validCatalogCache(cache) {
		t.Fatal("fresh cache invalid")
	}
	// 改动 raw 但保留 snapshot/hash：双 Hash 必须拒绝。
	bad := cache
	bad.RawBody = json.RawMessage(`{"instances":[]}`)
	if validCatalogCache(bad) {
		t.Fatal("raw/snapshot dual hash accepted tampered raw")
	}
	// 普通刷新命中 cache，不再打网络。
	if _, source, err := refreshModelIndex(ctx, false); err != nil || source != "cache" {
		t.Fatalf("cache hit=%q %v", source, err)
	}
	if f.http.Load() != 1 {
		t.Fatalf("cache hit did network: %d", f.http.Load())
	}
	// force + ETag/304。
	f.respond = func(req plugin.HTTPRequest) (plugin.HTTPResponse, error) {
		if req.Headers["If-None-Match"] != `"v1"` {
			t.Fatalf("If-None-Match=%q", req.Headers["If-None-Match"])
		}
		return plugin.HTTPResponse{Status: 304}, nil
	}
	if _, source, err := refreshModelIndex(ctx, true); err != nil || source != "cache-304" {
		t.Fatalf("304=%q %v", source, err)
	}
}

func TestCatalogRefreshHosttestBadResponsesFallbackEmbedded(t *testing.T) {
	cases := []struct {
		name string
		resp func(plugin.HTTPRequest) (plugin.HTTPResponse, error)
	}{
		{"transport", func(plugin.HTTPRequest) (plugin.HTTPResponse, error) {
			return plugin.HTTPResponse{}, errors.New("timeout")
		}},
		{"status500", func(plugin.HTTPRequest) (plugin.HTTPResponse, error) { return plugin.HTTPResponse{Status: 500}, nil }},
		{"badjson", func(plugin.HTTPRequest) (plugin.HTTPResponse, error) {
			return plugin.HTTPResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"bad":`))}, nil
		}},
		{"empty", func(plugin.HTTPRequest) (plugin.HTTPResponse, error) {
			return httpBody([]byte(`{"instances":[]}`)), nil
		}},
		{"oversize", func(plugin.HTTPRequest) (plugin.HTTPResponse, error) {
			// maxCatalogBytes+1 的合法 JSON 前缀 + 填充；长度检查在 JSON 解析前。
			b := make([]byte, maxCatalogBytes+1)
			copy(b, `{"instances":[`)
			return plugin.HTTPResponse{Status: 200, BodyB64: base64.StdEncoding.EncodeToString(b)}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, f := newCatalogFixture(t)
			f.respond = tc.resp
			idx, source, err := refreshModelIndex(ctx, false)
			if err != nil || source != "embedded" || idx == nil {
				t.Fatalf("fallback=%q idx=%v err=%v", source, idx != nil, err)
			}
		})
	}
}

func TestCatalogRefreshHosttestTTLAndEmbedded(t *testing.T) {
	ctx, f := newCatalogFixture(t)
	body := rawCatalogBody(t, "a.light")
	f.respond = func(plugin.HTTPRequest) (plugin.HTTPResponse, error) { return httpBody(body), nil }
	if _, source, _ := refreshModelIndex(ctx, false); source != "network" {
		t.Fatal(source)
	}
	// 改旧 FetchedAt 模拟 TTL 过期，网络坏响应 → embedded。
	var cache catalogCacheV2
	_ = json.Unmarshal(f.files["spec-cache/instances-v2.json"], &cache)
	cache.FetchedAt = "2000-01-01T00:00:00Z"
	cache.RawSHA256 = mustSHAHex(cache.RawBody)
	cache.SnapshotSHA256 = mustSHAHex(cache.Body)
	f.files["spec-cache/instances-v2.json"] = mustJSON(cache)
	f.respond = func(plugin.HTTPRequest) (plugin.HTTPResponse, error) { return plugin.HTTPResponse{Status: 500}, nil }
	if _, source, _ := refreshModelIndex(ctx, false); source != "embedded" {
		t.Fatalf("expired cache source=%s", source)
	}
	// 状态工具只读 cache，不触发网络。
	f.http.Store(0)
	if _, err := catalogStatusTool(ctx); err != nil {
		t.Fatal(err)
	}
	if f.http.Load() != 0 {
		t.Fatal("catalog_status performed network")
	}
}

func TestCatalogRefreshHosttestSingleflight(t *testing.T) {
	ctx, f := newCatalogFixture(t)
	body := rawCatalogBody(t, "a.light")
	f.block.Store(true)
	f.release = make(chan struct{})
	f.respond = func(plugin.HTTPRequest) (plugin.HTTPResponse, error) { return httpBody(body), nil }
	const n = 8
	var wg sync.WaitGroup
	results := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, source, err := refreshModelIndex(ctx, false)
			if err != nil {
				errs <- err
				return
			}
			results <- source
		}()
	}
	// 等第一个请求进入 handler（其余在 cond 上等待）。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && f.http.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if f.http.Load() != 1 {
		t.Fatalf("blocked requests=%d", f.http.Load())
	}
	close(f.release)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if f.http.Load() != 1 {
		t.Fatalf("singleflight HTTP calls=%d, want 1", f.http.Load())
	}
	for source := range results {
		if source != "network" && source != "cache" {
			t.Fatalf("unexpected source=%s", source)
		}
	}
}

func TestCatalogRefreshToolsAndBackgroundTick(t *testing.T) {
	ctx, f := newCatalogFixture(t)
	body := rawCatalogBody(t, "a.light", "airquality.air")
	f.respond = func(plugin.HTTPRequest) (plugin.HTTPResponse, error) { return httpBody(body), nil }
	// background tick 使用同一刷新路径。
	if err := event(ctx, "smart.plugin.resource.background.task.tick", mustJSON(map[string]any{"name": "catalog-refresh"})); err != nil {
		t.Fatal(err)
	}
	if f.http.Load() != 1 {
		t.Fatalf("background HTTP=%d", f.http.Load())
	}
	// catalog_status 只读；catalog_refresh 受 60s 频率限制。
	if _, err := call(ctx, "catalog_status", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := call(ctx, "catalog_refresh", nil); err == nil {
		t.Fatal("catalog_refresh rate limit not enforced")
	}
	// 重置后显式 refresh 成功（20s timeout 已在 fixture 中断言）。
	resetCatalogRefreshState()
	if _, err := call(ctx, "catalog_refresh", nil); err != nil {
		t.Fatal(err)
	}
	if f.http.Load() < 2 {
		t.Fatalf("catalog_refresh HTTP=%d", f.http.Load())
	}
}
