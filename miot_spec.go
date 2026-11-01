package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"homex.dev/homex/sdk/go/plugin"
)

//go:embed services.json
var embeddedMappingRules []byte

const specCacheVersion = 1
const specTTL = 7 * 24 * time.Hour

type miotSpec struct {
	Type     string        `json:"type"`
	Services []miotService `json:"services"`
}
type miotService struct {
	IID        int            `json:"iid"`
	Type       string         `json:"type"`
	Properties []miotProperty `json:"properties"`
	Actions    []miotAction   `json:"actions"`
	Events     []miotEvent    `json:"events"`
}
type miotValueItem struct {
	Value       int    `json:"value"`
	Description string `json:"description"`
}
type miotProperty struct {
	IID        int             `json:"iid"`
	Type       string          `json:"type"`
	Format     string          `json:"format"`
	Access     []string        `json:"access"`
	Unit       string          `json:"unit"`
	ValueRange []float64       `json:"value-range"`
	ValueList  []miotValueItem `json:"value-list"`
}
type miotAction struct {
	IID  int    `json:"iid"`
	Type string `json:"type"`
	In   []int  `json:"in"`
	Out  []int  `json:"out"`
}
type miotEvent struct {
	IID       int    `json:"iid"`
	Type      string `json:"type"`
	Arguments []int  `json:"arguments"`
}
type propertyBinding struct {
	PIID        int            `json:"piid"`
	Readable    bool           `json:"readable"`
	Writable    bool           `json:"writable"`
	Format      string         `json:"format,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Range       []float64      `json:"range,omitempty"`
	Values      map[int]string `json:"values,omitempty"`
	DeviceClass string         `json:"device_class,omitempty"`
}
type actionBinding struct {
	AIID    int        `json:"aiid"`
	Inputs  []int      `json:"inputs,omitempty"`
	Outputs []int      `json:"outputs,omitempty"`
	Rule    actionRule `json:"rule,omitempty"`
}
type eventBinding struct {
	EIID      int   `json:"eiid"`
	Arguments []int `json:"arguments,omitempty"`
}
type capabilityBinding struct {
	Capability   string                     `json:"capability"`
	SIID         int                        `json:"siid"`
	Properties   map[string]int             `json:"properties,omitempty"`
	PropertyMeta map[string]propertyBinding `json:"property_meta,omitempty"`
	Actions      map[string]int             `json:"actions,omitempty"`
	ActionMeta   map[string]actionBinding   `json:"action_meta,omitempty"`
	Events       map[string]eventBinding    `json:"events,omitempty"`
	ReadOnly     bool                       `json:"read_only,omitempty"`
}
type specCache struct {
	Version   int             `json:"version"`
	Model     string          `json:"model"`
	FetchedAt string          `json:"fetched_at"`
	SHA256    string          `json:"sha256"`
	Spec      json.RawMessage `json:"spec"`
}

func shortURN(s string) string {
	p := strings.Split(s, ":")
	if len(p) > 3 {
		return strings.ReplaceAll(p[3], "_", "-")
	}
	return s
}
func mapSpec(sp miotSpec) []capabilityBinding {
	out := []capabilityBinding{}
	for _, svc := range sp.Services {
		name := shortURN(svc.Type)
		cap := ""
		switch name {
		case "light", "indicator-light":
			cap = "light"
		case "switch", "outlet":
			cap = "switch"
		case "air-conditioner", "thermostat":
			cap = "climate"
		case "curtain", "window-opener":
			cap = "cover"
		case "environment", "sensor":
			cap = "sensor"
		}
		if cap == "" {
			continue
		}
		b := capabilityBinding{Capability: cap, SIID: svc.IID, Properties: map[string]int{}, Actions: map[string]int{}}
		writable := false
		for _, p := range svc.Properties {
			pn := shortURN(p.Type)
			b.Properties[pn] = p.IID
			for _, a := range p.Access {
				writable = writable || a == "write"
			}
		}
		for _, a := range svc.Actions {
			b.Actions[shortURN(a.Type)] = a.IID
		}
		b.ReadOnly = !writable && len(b.Actions) == 0
		out = append(out, b)
	}
	return out
}
// specLocks 按缓存路径做单飞：同一模型的并发命令只抓一次 Spec。
//
// 没有它时，10 个并发命令会发 10 次 HTTPS、写 10 次缓存（WASM 内 fs 写是
// 串行资源调用，平白放大时延）。锁只覆盖同一 model，不阻塞不同设备。
var (
	specLocksMu sync.Mutex
	specLocks   = map[string]*sync.Mutex{}
)

func specLock(path string) *sync.Mutex {
	specLocksMu.Lock()
	defer specLocksMu.Unlock()
	mu := specLocks[path]
	if mu == nil {
		mu = &sync.Mutex{}
		specLocks[path] = mu
	}
	return mu
}

func readCachedSpec(ctx *plugin.Context, path, model string) (miotSpec, bool) {
	r, e := ctx.FSRead(path)
	if e != nil || !r.Found {
		return miotSpec{}, false
	}
	b, e := r.Data()
	if e != nil {
		return miotSpec{}, false
	}
	var c specCache
	if json.Unmarshal(b, &c) != nil || c.Version != specCacheVersion || c.Model != model {
		return miotSpec{}, false
	}
	ts, _ := time.Parse(time.RFC3339, c.FetchedAt)
	sum := sha256.Sum256(c.Spec)
	if fmt.Sprintf("%x", sum[:]) != c.SHA256 || time.Since(ts) >= specTTL {
		return miotSpec{}, false
	}
	var sp miotSpec
	if json.Unmarshal(c.Spec, &sp) != nil {
		return miotSpec{}, false
	}
	return sp, true
}

func loadMIoTSpec(ctx *plugin.Context, model string) (miotSpec, error) {
	path := "spec-cache/" + safeModel(model) + ".json"
	if sp, ok := readCachedSpec(ctx, path, model); ok {
		return sp, nil
	}
	mu := specLock(path)
	mu.Lock()
	defer mu.Unlock()
	// 双检：等锁期间可能已被其它命令写入。
	if sp, ok := readCachedSpec(ctx, path, model); ok {
		return sp, nil
	}
	u := "https://miot-spec.org/miot-spec-v2/instance?type=" + model
	resp, e := ctx.HTTPRequest(plugin.HTTPRequest{Method: "GET", URL: u, TimeoutMS: 15000})
	if e != nil {
		return miotSpec{}, fmt.Errorf("spec网络失败且无有效缓存: %w", e)
	}
	body, e := resp.Body()
	if e != nil || resp.Status != 200 {
		return miotSpec{}, errors.New("spec响应无效")
	}
	var sp miotSpec
	if json.Unmarshal(body, &sp) != nil || len(sp.Services) == 0 {
		return miotSpec{}, errors.New("spec解析失败")
	}
	sum := sha256.Sum256(body)
	c := specCache{Version: specCacheVersion, Model: model, FetchedAt: time.Now().UTC().Format(time.RFC3339), SHA256: fmt.Sprintf("%x", sum[:]), Spec: body}
	cb, _ := json.Marshal(c)
	w := plugin.FSWriteRequest{Path: path, QuotaKB: 4096}
	w.SetData(cb)
	_ = ctx.FSWrite(w)
	return sp, nil
}
func safeModel(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}
func validateEmbeddedRules() error {
	var v map[string]any
	if json.Unmarshal(embeddedMappingRules, &v) != nil || len(v) == 0 {
		return errors.New("embedded mapping rules invalid")
	}
	return nil
}
