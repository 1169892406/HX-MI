package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"sort"
	"sync"
	"strings"
	"time"
)

type ModelIndex struct {
	exact   map[string]MIoTInstance
	aliases map[string]string
	hash    string
}

func statusRank(s string) int {
	switch s {
	case "released":
		return 3
	case "preview":
		return 2
	case "debug":
		return 1
	}
	return 0
}
func BuildModelIndex(items []MIoTInstance) (*ModelIndex, error) {
	idx := &ModelIndex{exact: map[string]MIoTInstance{}, aliases: map[string]string{}}
	sorted := append([]MIoTInstance(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Model != sorted[j].Model {
			return sorted[i].Model < sorted[j].Model
		}
		if sorted[i].Version != sorted[j].Version {
			return sorted[i].Version > sorted[j].Version
		}
		return statusRank(sorted[i].Status) > statusRank(sorted[j].Status)
	})
	for _, x := range sorted {
		if strings.TrimSpace(x.Model) == "" || strings.TrimSpace(x.Type) == "" || x.Version < 0 {
			return nil, errors.New("catalog item missing/invalid fields")
		}
		if old, ok := idx.exact[x.Model]; ok {
			if old.Version == x.Version && old.Type != x.Type {
				return nil, fmt.Errorf("conflicting model %s v%d", x.Model, x.Version)
			}
			continue
		}
		idx.exact[x.Model] = x
		a := modelAlias(x.Model)
		if a != x.Model {
			if prev, ok := idx.aliases[a]; ok && prev != x.Model {
				delete(idx.aliases, a)
			} else {
				idx.aliases[a] = x.Model
			}
		}
	}
	raw, _ := json.Marshal(sorted)
	sum := sha256.Sum256(raw)
	idx.hash = fmt.Sprintf("%x", sum[:])
	return idx, nil
}
func modelAlias(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return s
}
func (i *ModelIndex) Resolve(model string) (MIoTInstance, bool) {
	x, ok := i.exact[model]
	if ok {
		return x, true
	}
	if full, ok := i.aliases[model]; ok {
		x, ok = i.exact[full]
		return x, ok
	}
	return x, false
}

type catalogCache struct {
	Version   int             `json:"version"`
	FetchedAt string          `json:"fetched_at"`
	SHA256    string          `json:"sha256"`
	Body      json.RawMessage `json:"body"`
}

// modelIndexCache 缓存按代际构建的 46k 型号索引。
//
// t234：早期每条 driver.command 都走 loadModelIndex → 解析 + 排序 + 校验
// 四万余条目录，单条命令的 CPU 时间足以吃满执行预算（60s 超时、HTTP 计数
// 为 0、ops 停在启动序列），现场表现为"命令挂起"。索引只依赖目录内容，
// 与账号/家庭无关，进程内缓存即可；目录刷新时用 ResetModelIndexCache 失效。
var (
	modelIndexMu    sync.Mutex
	modelIndexCached *ModelIndex
	modelIndexKey   string
)

// ResetModelIndexCache 让下次 loadModelIndex 重新构建（目录刷新后调用）。
func ResetModelIndexCache() {
	modelIndexMu.Lock()
	modelIndexCached = nil
	modelIndexKey = ""
	modelIndexMu.Unlock()
}

func loadModelIndex(ctx *plugin.Context) (*ModelIndex, error) {
	idx, key, e := loadModelIndexFunc(ctx)
	if e != nil {
		return nil, e
	}
	modelIndexMu.Lock()
	if modelIndexCached != nil && modelIndexKey == key {
		cached := modelIndexCached
		modelIndexMu.Unlock()
		return cached, nil
	}
	modelIndexCached, modelIndexKey = idx, key
	modelIndexMu.Unlock()
	return idx, nil
}

// embeddedJSONForCache 把已解析的 snapshot 归一化成稳定字节，用作缓存键。
func embeddedJSONForCache(items []MIoTInstance) []byte {
	b, _ := json.Marshal(items)
	return b
}

func loadModelIndexFunc(ctx *plugin.Context) (*ModelIndex, string, error) {
	embedded, e := loadMIoTInstanceSnapshot()
	if e != nil {
		return nil, "", e
	}
	fallback, e := BuildModelIndex(embedded)
	if e != nil {
		return nil, "", e
	}
	// 键 = 缓存内容摘要（raw body 的 SHA256 + fetched_at）；无缓存时用
	// embedded 摘要，保证"同一份目录只构建一次"。
	embeddedKey := "embedded"
	if len(embedded) > 0 {
		sum := sha256.Sum256(embeddedJSONForCache(embedded))
		embeddedKey = fmt.Sprintf("embedded:%x", sum[:8])
	}
	r, e := ctx.FSRead("spec-cache/instances.json")
	if e != nil || !r.Found {
		return fallback, embeddedKey, nil
	}
	b, e := r.Data()
	if e != nil {
		return fallback, embeddedKey, nil
	}
	var c catalogCache
	if json.Unmarshal(b, &c) != nil || c.Version != 1 {
		return fallback, embeddedKey, nil
	}
	sum := sha256.Sum256(c.Body)
	ts, _ := time.Parse(time.RFC3339, c.FetchedAt)
	if c.SHA256 != fmt.Sprintf("%x", sum[:]) || time.Since(ts) >= 24*time.Hour {
		return fallback, embeddedKey, nil
	}
	var doc miotInstanceSnapshot
	if json.Unmarshal(c.Body, &doc) != nil {
		return fallback, embeddedKey, nil
	}
	idx, e := BuildModelIndex(doc.Instances)
	if e != nil {
		return fallback, embeddedKey, nil
	}
	return idx, "cache:" + c.SHA256, nil
}

func (i *ModelIndex) Coverage(rules *ruleIndex) (mapped int, unmapped []string) {
	for _, x := range i.exact {
		typ := normalizeName(shortURN(x.Type))
		if _, ok := rules.services[typ]; ok {
			mapped++
			continue
		}
		if _, ok := rules.sensors[typ]; ok {
			mapped++
			continue
		}
		unmapped = append(unmapped, x.Model)
	}
	sort.Strings(unmapped)
	return
}
