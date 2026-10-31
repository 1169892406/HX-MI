package main

import (
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
	"strings"
	"sync"
)

var bindingCache sync.Map

func resolveCommandBinding(ctx *plugin.Context, d miioDevice, c miioCommand) (miioCommand, error) {
	if strings.TrimSpace(d.Model) == "" {
		return c, errors.New("设备model缺失")
	}
	typ, ok := lookupModelType(d.Model)
	if !ok {
		return c, fmt.Errorf("未知model %s", d.Model)
	}
	key := d.Model + "|" + typ
	if v, ok := bindingCache.Load(key); ok {
		return injectCommand(c, v.([]capabilityBinding))
	}
	sp, e := loadMIoTSpec(ctx, typ)
	if e != nil {
		return c, e
	}
	rules, e := cachedRuleIndex()
	if e != nil {
		return c, e
	}
	bs := rules.bind(sp)
	if len(bs) == 0 {
		return c, errors.New("model无可用Binding")
	}
	bindingCache.Store(key, bs)
	return injectCommand(c, bs)
}
func injectCommand(c miioCommand, bs []capabilityBinding) (miioCommand, error) {
	capName := normalizeName(c.Capability)
	for _, b := range bs {
		if capName != "" && normalizeName(b.Capability) != capName {
			continue
		}
		action := normalizeName(c.Action)
		if am, ok := b.ActionMeta[action]; ok && am.AIID > 0 {
			p := copyParams(c.Params)
			p["siid"] = b.SIID
			p["aiid"] = am.AIID
			p["in"] = am.Inputs
			c.Params = p
			return c, nil
		}
		propName := fmt.Sprint(c.Params["property"])
		if propName == "" || propName == "<nil>" {
			propName = actionProperty(action)
		}
		if meta, ok := b.PropertyMeta[propName]; ok {
			p := copyParams(c.Params)
			p["siid"] = b.SIID
			p["piid"] = meta.PIID
			p["meta"] = meta
			if ar, ok := b.ActionMeta[action]; ok {
				p["encode"] = ar.Rule.Encode
			}
			c.Params = p
			return c, nil
		}
	}
	return c, fmt.Errorf("Binding不支持 capability=%s action=%s", c.Capability, c.Action)
}
func actionProperty(a string) string {
	switch a {
	case "turn-on", "turn-off", "toggle":
		return "on"
	case "set-brightness":
		return "brightness"
	case "set-color":
		return "color-hex"
	case "set-color-temp":
		return "color-temp-kelvin"
	}
	return a
}
func copyParams(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
