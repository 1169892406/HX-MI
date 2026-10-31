package main

import (
	"encoding/json"
	"errors"
	"homex.dev/homex/sdk/go/plugin"
	"time"
)

type miotEventEnvelope struct {
	ExternalID string `json:"external_id"`
	Model      string `json:"model"`
	SIID       int    `json:"siid"`
	EIID       int    `json:"eiid"`
	Args       []any  `json:"args"`
}

func handleMIoTEvent(ctx *plugin.Context, raw json.RawMessage) error {
	var e miotEventEnvelope
	if json.Unmarshal(raw, &e) != nil || e.ExternalID == "" || e.Model == "" {
		return errors.New("event envelope非法")
	}
	typ, ok := lookupModelType(e.Model)
	if !ok {
		return errors.New("event model未知")
	}
	sp, er := loadMIoTSpec(ctx, typ)
	if er != nil {
		return er
	}
	rules, er := cachedRuleIndex()
	if er != nil {
		return er
	}
	for _, b := range rules.bind(sp) {
		if b.SIID != e.SIID {
			continue
		}
		for name, ev := range b.Events {
			if ev.EIID == e.EIID {
				state, er := decodeEvent(ev, e.Args)
				if er != nil {
					return er
				}
				state["event"] = name
				return ctx.DeviceStatePublish(plugin.DeviceStateRequest{ExternalID: e.ExternalID, State: state, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
			}
		}
	}
	return errors.New("event Binding未知")
}
