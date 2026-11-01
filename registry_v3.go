package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"homex.dev/homex/sdk/go/plugin"
)

func persistNumericDeviceID(ctx *plugin.Context, external string, id uint32) error {
	if id == 0 {
		return errors.New("numeric device id invalid")
	}
	ds, e := loadDeviceRegistry(ctx)
	if e != nil {
		return e
	}
	found := false
	for i := range ds {
		if ds[i].ExternalID == external {
			ds[i].NumericDeviceID = id
			found = true
		} else if ds[i].NumericDeviceID == id {
			return errors.New("numeric device id conflict")
		}
	}
	if !found {
		return errors.New("device not registered")
	}
	payload, _ := json.Marshal(ds)
	sum := sha256.Sum256(payload)
	b, _ := json.Marshal(deviceRegistryV2{Version: 3, Devices: ds, SHA256: fmt.Sprintf("%x", sum[:])})
	w := plugin.FSWriteRequest{Path: "devices.json", QuotaKB: 4096}
	w.SetData(b)
	return ctx.FSWrite(w)
}
func externalByPacketDeviceID(ds []miioDevice, pkt []byte) (miioDevice, bool) {
	if len(pkt) < 12 {
		return miioDevice{}, false
	}
	id := binary.BigEndian.Uint32(pkt[8:12])
	if id == 0 {
		return miioDevice{}, false
	}
	for _, d := range ds {
		if d.NumericDeviceID == id {
			return d, true
		}
	}
	return miioDevice{}, false
}
