package main

import (
	"fmt"
	"sort"

	"homex.dev/homex/sdk/go/plugin"
)

// capabilitiesFromBindings 从 Spec/Map Binding 投影出设备能力面。
//
// 为什么需要它：Driver announce 的 capabilities 是平台/前端决定"这台设备
// 能显示哪些控件"的唯一依据。旧实现完全信 devices.json 里旧服务写下的
// capabilities；模型升级或旧数据缺字段时，设备会在 UI 上"消失能力"。
// Spec binding 才是新实现的权威能力来源，因此在迁移/announce 时投影一次。
func capabilitiesFromBindings(bs []capabilityBinding) []string {
	set := map[string]struct{}{}
	for _, b := range bs {
		if b.Capability == "" {
			continue
		}
		set[b.Capability] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// projectCapabilities 按 model 解析 Spec/Map 并投影能力；失败时返回错误，
// 调用方应保留旧能力而不是中断迁移。
func projectCapabilities(ctx *plugin.Context, model string) ([]string, error) {
	if model == "" {
		return nil, fmt.Errorf("model 为空")
	}
	typ, ok := lookupModelType(model)
	if !ok {
		return nil, fmt.Errorf("未知 model %s", model)
	}
	sp, err := loadMIoTSpec(ctx, typ)
	if err != nil {
		return nil, err
	}
	rules, err := cachedRuleIndex()
	if err != nil {
		return nil, err
	}
	bs := rules.bind(sp)
	if len(bs) == 0 {
		return nil, fmt.Errorf("model %s 无可用 Binding", model)
	}
	return capabilitiesFromBindings(bs), nil
}

// mergeCapabilities 合并旧声明与投影能力：旧值优先保留（兼容历史 UI 选择），
// 投影补足缺失项；结果去重且稳定排序。
func mergeCapabilities(legacy, projected []string) []string {
	set := map[string]struct{}{}
	for _, c := range legacy {
		if c != "" {
			set[c] = struct{}{}
		}
	}
	for _, c := range projected {
		if c != "" {
			set[c] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
