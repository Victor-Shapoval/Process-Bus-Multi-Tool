package goose

// CloneDataValues detaches every mutable payload from the original values.
func CloneDataValues(values []DataValue) []DataValue {
	if values == nil {
		return nil
	}
	out := make([]DataValue, len(values))
	for i, value := range values {
		out[i] = value
		out[i].Bytes = append([]byte(nil), value.Bytes...)
		out[i].Children = CloneDataValues(value.Children)
	}
	return out
}

func ClonePDU(p *PDU) *PDU {
	if p == nil {
		return nil
	}
	out := *p
	out.DstMAC = append([]byte(nil), p.DstMAC...)
	out.SrcMAC = append([]byte(nil), p.SrcMAC...)
	out.AllData = CloneDataValues(p.AllData)
	if p.VLAN != nil {
		vlan := *p.VLAN
		out.VLAN = &vlan
	}
	return &out
}
