package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"sort"
	"strings"
	"sync"
	"time"
)

const catalogURL = "https://miot-spec.org/miot-spec-v2/instances?status=all"
const maxCatalogBytes = 32 << 20

var catalogRefreshMu sync.Mutex

type catalogCacheV2 struct {
	Version        int             `json:"version"`
	ETag           string          `json:"etag,omitempty"`
	FetchedAt      string          `json:"fetched_at"`
	RawSHA256      string          `json:"raw_sha256"`
	SnapshotSHA256 string          `json:"snapshot_sha256"`
	RawBody        json.RawMessage `json:"raw_body"`
	Body           json.RawMessage `json:"body"`
}

func refreshModelIndex(ctx *plugin.Context, force bool) (*ModelIndex, string, error) {
	// 单飞：同一时刻只允许一个真实刷新；并发调用者等它完成后直接读 cache，
	// 不能各自再打一次上游（t187 并发单飞）。
	catalogRefreshMu.Lock()
	if catalogRefreshing {
		for catalogRefreshing {
			catalogRefreshCond.Wait()
		}
		catalogRefreshMu.Unlock()
		// 单飞等待者不再打上游：第一个请求已写 cache（或 fallback embedded）。
		return catalogIndexFromCacheOrEmbedded(ctx)
	}
	catalogRefreshing = true
	catalogRefreshMu.Unlock()
	defer func() {
		catalogRefreshMu.Lock()
		catalogRefreshing = false
		catalogRefreshCond.Broadcast()
		catalogRefreshMu.Unlock()
	}()
	idx, source, err := refreshModelIndexOnce(ctx, force)
	catalogRefreshMu.Lock()
	if err == nil {
		catalogLastRefresh = time.Now()
		catalogRefreshCount++
	} else {
		catalogRefreshFailures++
	}
	catalogRefreshMu.Unlock()
	return idx, source, err
}

func catalogIndexFromCacheOrEmbedded(ctx *plugin.Context) (*ModelIndex, string, error) {
	embedded, err := loadMIoTInstanceSnapshot()
	if err != nil {
		return nil, "", err
	}
	fallback, err := BuildModelIndex(embedded)
	if err != nil {
		return nil, "", err
	}
	r, err := ctx.FSRead("spec-cache/instances-v2.json")
	if err != nil || !r.Found {
		return fallback, "embedded", nil
	}
	b, err := r.Data()
	if err != nil {
		return fallback, "embedded", nil
	}
	var c catalogCacheV2
	if json.Unmarshal(b, &c) != nil || !validCatalogCache(c) {
		return fallback, "embedded", nil
	}
	var d miotInstanceSnapshot
	if json.Unmarshal(c.Body, &d) != nil {
		return fallback, "embedded", nil
	}
	idx, err := BuildModelIndex(d.Instances)
	if err != nil {
		return fallback, "embedded", nil
	}
	return idx, "cache", nil
}

func refreshModelIndexOnce(ctx *plugin.Context, force bool) (*ModelIndex, string, error) {
	fallbackItems, e := loadMIoTInstanceSnapshot()
	if e != nil {
		return nil, "", e
	}
	fallback, _ := BuildModelIndex(fallbackItems)
	var cached catalogCacheV2
	if r, e := ctx.FSRead("spec-cache/instances-v2.json"); e == nil && r.Found {
		if b, e := r.Data(); e == nil {
			_ = json.Unmarshal(b, &cached)
			if validCatalogCache(cached) && !force {
				var d miotInstanceSnapshot
				_ = json.Unmarshal(cached.Body, &d)
				idx, _ := BuildModelIndex(d.Instances)
				return idx, "cache", nil
			}
		}
	}
	headers := map[string]string{}
	if cached.ETag != "" {
		headers["If-None-Match"] = cached.ETag
	}
	resp, e := ctx.HTTPRequest(plugin.HTTPRequest{Method: "GET", URL: catalogURL, Headers: headers, TimeoutMS: 20000})
	if e != nil {
		return fallback, "embedded", nil
	}
	if resp.Status == 304 && validCatalogCache(cached) {
		var d miotInstanceSnapshot
		_ = json.Unmarshal(cached.Body, &d)
		idx, _ := BuildModelIndex(d.Instances)
		return idx, "cache-304", nil
	}
	body, e := resp.Body()
	if e != nil || resp.Status != 200 || len(body) > maxCatalogBytes {
		return fallback, "embedded", nil
	}
	var doc miotInstanceSnapshot
	if json.Unmarshal(body, &doc) != nil || len(doc.Instances) == 0 {
		return fallback, "embedded", nil
	}
	idx, e := BuildModelIndex(doc.Instances)
	if e != nil {
		return fallback, "embedded", nil
	}
	rawSum := sha256.Sum256(body)
	snapshot, _ := json.Marshal(doc)
	snapSum := sha256.Sum256(snapshot)
	c := catalogCacheV2{Version: 2, ETag: resp.Headers["ETag"], FetchedAt: time.Now().UTC().Format(time.RFC3339), RawSHA256: fmt.Sprintf("%x", rawSum[:]), SnapshotSHA256: fmt.Sprintf("%x", snapSum[:]), RawBody: append(json.RawMessage(nil), body...), Body: snapshot}
	cb, _ := json.Marshal(c)
	w := plugin.FSWriteRequest{Path: "spec-cache/instances-v2.json", QuotaKB: 131072}
	w.SetData(cb)
	_ = ctx.FSWrite(w)
	return idx, "network", nil
}

// catalogCacheStatus 返回无网络的 cache 视图（tool/status/后台日志用）。
type catalogCacheStatus struct {
	Found          bool   `json:"found"`
	Valid          bool   `json:"valid"`
	ETag           string `json:"etag,omitempty"`
	FetchedAt      string `json:"fetched_at,omitempty"`
	AgeSeconds     int64  `json:"age_seconds,omitempty"`
	Refreshes      int    `json:"refreshes"`
	Failures       int    `json:"failures"`
	LastRefresh    string `json:"last_refresh,omitempty"`
	RawSHA256      string `json:"raw_sha256,omitempty"`
	SnapshotSHA256 string `json:"snapshot_sha256,omitempty"`
}

func readCatalogCacheStatus(ctx *plugin.Context) catalogCacheStatus {
	var c catalogCacheV2
	r, e := ctx.FSRead("spec-cache/instances-v2.json")
	if e != nil || !r.Found {
		return catalogCacheStatus{}
	}
	b, e := r.Data()
	if e != nil || json.Unmarshal(b, &c) != nil {
		return catalogCacheStatus{}
	}
	out := catalogCacheStatus{Found: true, Valid: validCatalogCache(c), ETag: c.ETag, FetchedAt: c.FetchedAt, RawSHA256: c.RawSHA256, SnapshotSHA256: c.SnapshotSHA256}
	if ts, e := time.Parse(time.RFC3339, c.FetchedAt); e == nil {
		out.AgeSeconds = int64(time.Since(ts).Seconds())
	}
	catalogRefreshMu.Lock()
	out.Refreshes = catalogRefreshCount
	out.Failures = catalogRefreshFailures
	if !catalogLastRefresh.IsZero() {
		out.LastRefresh = catalogLastRefresh.UTC().Format(time.RFC3339)
	}
	catalogRefreshMu.Unlock()
	return out
}

var (
	catalogRefreshCond     = sync.NewCond(&catalogRefreshMu)
	catalogRefreshing      bool
	catalogLastRefresh     time.Time
	catalogRefreshCount    int
	catalogRefreshFailures int
)

func validCatalogCache(c catalogCacheV2) bool {
	if c.Version != 2 || len(c.Body) == 0 || len(c.RawBody) == 0 {
		return false
	}
	ts, e := time.Parse(time.RFC3339, c.FetchedAt)
	if e != nil || time.Since(ts) >= 24*time.Hour {
		return false
	}
	raw := sha256.Sum256(c.RawBody)
	if !strings.EqualFold(c.RawSHA256, fmt.Sprintf("%x", raw[:])) {
		return false
	}
	snap := sha256.Sum256(c.Body)
	return strings.EqualFold(c.SnapshotSHA256, fmt.Sprintf("%x", snap[:]))
}

type coverageReport struct {
	Version        int            `json:"version"`
	GeneratedAt    string         `json:"generated_at"`
	TotalModels    int            `json:"total_models"`
	Mapped         int            `json:"mapped"`
	Unmapped       int            `json:"unmapped"`
	ByStatus       map[string]int `json:"by_status"`
	ByKind         map[string]int `json:"by_kind"`
	AliasConflicts []string       `json:"alias_conflicts,omitempty"`
	UnmappedModels []string       `json:"unmapped_models"`
}

func buildCoverageReport(idx *ModelIndex, rules *ruleIndex) coverageReport {
	r := coverageReport{Version: 1, GeneratedAt: "deterministic", TotalModels: len(idx.exact), ByStatus: map[string]int{}, ByKind: map[string]int{}}
	aliases := map[string][]string{}
	for model, x := range idx.exact {
		r.ByStatus[x.Status]++
		aliases[modelAlias(model)] = append(aliases[modelAlias(model)], model)
		typ := normalizeName(shortURN(x.Type))
		kind := "unmapped"
		if _, ok := rules.airQuality[typ]; ok {
			kind = "air_quality"
		} else if _, ok := rules.services[typ]; ok {
			kind = "services"
		} else if _, ok := rules.sensors[typ]; ok {
			kind = "sensors"
		} else if _, ok := rules.degrade[typ]; ok {
			kind = "degradations"
		}
		if kind == "unmapped" {
			r.UnmappedModels = append(r.UnmappedModels, model)
			r.Unmapped++
		} else {
			r.Mapped++
		}
		r.ByKind[kind]++
	}
	for a, models := range aliases {
		if len(models) > 1 {
			sort.Strings(models)
			r.AliasConflicts = append(r.AliasConflicts, a+":"+strings.Join(models, ","))
		}
	}
	sort.Strings(r.AliasConflicts)
	sort.Strings(r.UnmappedModels)
	return r
}
func writeCoverageReport(ctx *plugin.Context, r coverageReport) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 8<<20 {
		return errors.New("coverage report too large")
	}
	w := plugin.FSWriteRequest{Path: "spec-cache/coverage.json", QuotaKB: 65536}
	w.SetData(b)
	return ctx.FSWrite(w)
}

const catalogToolMinInterval = 60 * time.Second

func catalogStatusTool(ctx *plugin.Context) (json.RawMessage, error) {
	st := readCatalogCacheStatus(ctx)
	idx, source, err := loadCatalogIndexForStatus(ctx)
	if err != nil {
		return nil, err
	}
	rep := coverageForIndex(ctx, idx)
	return json.Marshal(map[string]any{
		"cache": st, "source": source, "models": len(idx.exact),
		"coverage": rep,
	})
}

func catalogRefreshTool(ctx *plugin.Context) (json.RawMessage, error) {
	catalogRefreshMu.Lock()
	last := catalogLastRefresh
	catalogRefreshMu.Unlock()
	if !last.IsZero() && time.Since(last) < catalogToolMinInterval {
		return nil, fmt.Errorf("catalog refresh rate limited: last=%s min=%s",
			last.UTC().Format(time.RFC3339), catalogToolMinInterval)
	}
	idx, source, err := refreshModelIndex(ctx, true)
	if err != nil {
		return nil, err
	}
	st := readCatalogCacheStatus(ctx)
	rep := coverageForIndex(ctx, idx)
	_ = writeCoverageReport(ctx, rep)
	ctx.Log(0, "hx-mi catalog tool refresh: "+source)
	return json.Marshal(map[string]any{
		"refreshed": true, "source": source, "models": len(idx.exact),
		"cache": st, "coverage": rep,
	})
}

func loadCatalogIndexForStatus(ctx *plugin.Context) (*ModelIndex, string, error) {
	embedded, err := loadMIoTInstanceSnapshot()
	if err != nil {
		return nil, "", err
	}
	fallback, err := BuildModelIndex(embedded)
	if err != nil {
		return nil, "", err
	}
	r, err := ctx.FSRead("spec-cache/instances-v2.json")
	if err != nil || !r.Found {
		return fallback, "embedded", nil
	}
	b, err := r.Data()
	if err != nil {
		return fallback, "embedded", nil
	}
	var c catalogCacheV2
	if json.Unmarshal(b, &c) != nil || !validCatalogCache(c) {
		return fallback, "embedded", nil
	}
	var d miotInstanceSnapshot
	if json.Unmarshal(c.Body, &d) != nil {
		return fallback, "embedded", nil
	}
	idx, err := BuildModelIndex(d.Instances)
	if err != nil {
		return fallback, "embedded", nil
	}
	return idx, "cache", nil
}

func coverageForIndex(_ *plugin.Context, idx *ModelIndex) coverageReport {
	rules, err := cachedRuleIndex()
	if err != nil {
		return coverageReport{Version: 1, GeneratedAt: "deterministic"}
	}
	return buildCoverageReport(idx, rules)
}
