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
	// Name 是 multiLanguage 接口给的中文名（英文接口没有这个字段）。
	Name string `json:"name"`
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
	Name       string          `json:"name"`
	Format     string          `json:"format"`
	Access     []string        `json:"access"`
	Unit       string          `json:"unit"`
	ValueRange []float64       `json:"value-range"`
	ValueList  []miotValueItem `json:"value-list"`
}
type miotAction struct {
	IID  int    `json:"iid"`
	Type string `json:"type"`
	Name string `json:"name"`
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


// applySpecTranslations 用翻译字典给服务/属性/动作补中文名（失败不影响功能）。
//
// 字典形状：{"data":{"zh":{"service:002":"音箱","service:002:action:003":"播放"}}}
// 键的编号是**3 位补零**（参考实现 MiotSpecInstance 的 unique_name 也是这么拼的）。
func applySpecTranslations(ctx *plugin.Context, sp *miotSpec, model string) {
	resp, err := ctx.HTTPRequest(plugin.HTTPRequest{
		Method: "GET", URL: "https://miot-spec.org/instance/v2/multiLanguage?urn=" + model, TimeoutMS: 12000})
	if err != nil {
		return
	}
	body, berr := resp.Body()
	if berr != nil || resp.Status != 200 {
		return
	}
	var wrapped struct {
		Data map[string]map[string]string `json:"data"`
	}
	if json.Unmarshal(body, &wrapped) != nil {
		return
	}
	zh := wrapped.Data["zh"]
	if len(zh) == 0 {
		zh = wrapped.Data["zh_cn"]
	}
	if len(zh) == 0 {
		return
	}
	key := func(parts ...any) string {
		b := ""
		for i, p := range parts {
			if i > 0 {
				b += ":"
			}
			b += fmt.Sprintf("%03d", p)
		}
		return b
	}
	for si := range sp.Services {
		svc := &sp.Services[si]
		if n, ok := zh["service:"+key(svc.IID)]; ok && n != "" {
			svc.Name = n
		}
		for pi := range svc.Properties {
			pr := &svc.Properties[pi]
			if n, ok := zh["service:"+key(svc.IID)+":property:"+key(pr.IID)]; ok && n != "" {
				pr.Name = n
			}
		}
		for ai := range svc.Actions {
			a := &svc.Actions[ai]
			if n, ok := zh["service:"+key(svc.IID)+":action:"+key(a.IID)]; ok && n != "" {
				a.Name = n
			}
		}
	}
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
	/*
	 * 规格结构与**中文名**分两个接口（2026-11-17 实测，与参考实现一致）：
	 *
	 *   /miot-spec-v2/instance?type=<urn>              → 结构与英文 description
	 *   /instance/v2/multiLanguage?urn=<urn>            → 翻译字典 {"data":{"zh":{...}}}
	 *
	 * 参考实现 miot_spec.py 也是这么拿的：结构 + 单独的 langs 请求
	 * （`dat.get('data')` 拿字典，键形如 `service:002:action:001`）。
	 *
	 * 为什么必须拿中文：界面上要显示「播放电台」而不是「Play Radio」——
	 * 用户对着英文名根本认不出那是干什么的。
	 */
	resp, e := ctx.HTTPRequest(plugin.HTTPRequest{
		Method: "GET", URL: "https://miot-spec.org/miot-spec-v2/instance?type=" + model, TimeoutMS: 15000})
	if e != nil {
		return miotSpec{}, fmt.Errorf("spec网络失败且无有效缓存: %w", e)
	}
	body, e := resp.Body()
	if e != nil || resp.Status != 200 {
		return miotSpec{}, errors.New("spec响应无效")
	}
	var sp miotSpec
	if json.Unmarshal(body, &sp) != nil || len(sp.Services) == 0 {
		/*
		 * multiLanguage 接口把规格包在 `{"code":0,"data":{...}}` 里
		 * （参考实现 miot_spec.py:457 用的就是 `dat.get('data')`）。
		 * 这里兼容两种形状：先按裸规格解，失败再剥一层 data。
		 */
		var wrapped struct {
			Code int       `json:"code"`
			Data miotSpec  `json:"data"`
		}
		if json.Unmarshal(body, &wrapped) != nil || len(wrapped.Data.Services) == 0 {
			return miotSpec{}, errors.New("spec解析失败")
		}
		sp = wrapped.Data
	}
	if len(sp.Services) == 0 {
		return miotSpec{}, errors.New("spec解析失败")
	}
	applySpecTranslations(ctx, &sp, model)
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
