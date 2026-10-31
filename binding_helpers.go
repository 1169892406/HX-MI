package main

func propertyMeta(p miotProperty) propertyBinding {
	b := propertyBinding{PIID: p.IID, Format: p.Format, Unit: p.Unit, Range: append([]float64(nil), p.ValueRange...), Values: map[int]string{}}
	for _, a := range p.Access {
		b.Readable = b.Readable || a == "read"
		b.Writable = b.Writable || a == "write"
	}
	for _, v := range p.ValueList {
		b.Values[v.Value] = v.Description
	}
	if len(b.Values) == 0 {
		b.Values = nil
	}
	return b
}
func enrichBinding(b *capabilityBinding, svc miotService, r serviceRule) {
	if b.PropertyMeta == nil {
		b.PropertyMeta = map[string]propertyBinding{}
	}
	if b.ActionMeta == nil {
		b.ActionMeta = map[string]actionBinding{}
	}
	if b.Events == nil {
		b.Events = map[string]eventBinding{}
	}
	for _, p := range svc.Properties {
		pn := normalizeName(shortURN(p.Type))
		if capProp, ok := r.Props[pn]; ok {
			b.PropertyMeta[capProp] = propertyMeta(p)
		}
	}
	for _, a := range svc.Actions {
		name := normalizeName(shortURN(a.Type))
		rule := r.Actions[name]
		b.Actions[name] = a.IID
		b.ActionMeta[name] = actionBinding{AIID: a.IID, Inputs: append([]int(nil), a.In...), Outputs: append([]int(nil), a.Out...), Rule: rule}
	}
	for _, e := range svc.Events {
		name := normalizeName(shortURN(e.Type))
		b.Events[name] = eventBinding{EIID: e.IID, Arguments: append([]int(nil), e.Arguments...)}
	}
}
