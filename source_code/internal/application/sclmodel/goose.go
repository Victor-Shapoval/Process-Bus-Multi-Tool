package sclmodel

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"

	"pbmt/internal/domain/goose"
)

// GooseStream is an SCL description, separate from reception configuration.
type GooseStream struct {
	IED, AccessPoint, Name, GocbRef, DatSet, GoID string
	DstMAC                                        string
	AppID                                         uint16
	ConfRev                                       uint32
	VLANID                                        *uint16
	VLANPriority                                  *uint8
	Entries                                       []*Type
	FrameError, SchemaError                       string
}

// ParseGooseSCL reads only GOOSE declarations; unrelated MMS/report/SV
// declarations must not prevent importing a valid GOOSE stream.
func ParseGooseSCL(data []byte) ([]GooseStream, error) {
	root, err := readSCLRoot(data)
	if err != nil {
		return nil, err
	}
	b := &importBuilder{types: map[string]xmlElement{}, budget: 200000}
	for _, typ := range childNamed(root, "DataTypeTemplates").Children {
		id := attribute(typ, "id")
		if id == "" || b.types[id].Name != "" {
			return nil, fmt.Errorf("missing or duplicate SCL type %q", id)
		}
		b.types[id] = typ
	}
	// Enum values are encoded as MMS INTEGER. Preserve the declared enum
	// schema elsewhere; the Subscriber only needs its wire type for matching.
	for id, typ := range b.types {
		for i, a := range typ.Children {
			if attribute(a, "bType") == "Enum" {
				if b.types[attribute(a, "type")].Name != "EnumType" {
					continue
				}
				setAttribute(&typ.Children[i], "bType", "INT32")
			}
		}
		b.types[id] = typ
	}
	var streams []GooseStream
	for _, ied := range childrenNamed(root, "IED") {
		for _, ap := range childrenNamed(ied, "AccessPoint") {
			devices := childrenNamed(childNamed(ap, "Server"), "LDevice")
			ldByInst := map[string]xmlElement{}
			domains := map[string]string{}
			for _, ld := range devices {
				inst := attribute(ld, "inst")
				if inst == "" || domains[inst] != "" {
					return nil, fmt.Errorf("missing or duplicate LDevice inst %q", inst)
				}
				domain := attribute(ld, "ldName")
				if domain == "" {
					domain = attribute(ied, "name") + inst
				}
				domains[inst], ldByInst[inst] = domain, ld
			}
			for _, ld := range devices {
				for _, ln := range ld.Children {
					if ln.Name != "LN0" && ln.Name != "LN" {
						continue
					}
					lnName := sclLNName(ln)
					for _, cb := range childrenNamed(ln, "GSEControl") {
						if kind := attribute(cb, "type"); kind != "" && kind != "GOOSE" {
							continue
						}
						g := GooseStream{IED: attribute(ied, "name"), AccessPoint: attribute(ap, "name"), Name: attribute(cb, "name"), GoID: attribute(cb, "appID")}
						g.GocbRef = domains[attribute(ld, "inst")] + "/" + lnName + "$GO$" + g.Name
						g.DatSet = domains[attribute(ld, "inst")] + "/" + lnName + "$" + attribute(cb, "datSet")
						rev, err := strconv.ParseUint(attribute(cb, "confRev"), 10, 32)
						if err != nil || g.Name == "" || attribute(cb, "datSet") == "" {
							g.FrameError = "missing or invalid GSEControl name, datSet or confRev"
						}
						g.ConfRev = uint32(rev)
						if err := readGooseAddress(&g, root, attribute(ld, "inst")); err != nil {
							g.FrameError = err.Error()
						}
						var sets []xmlElement
						for _, ds := range childrenNamed(ln, "DataSet") {
							if attribute(ds, "name") == attribute(cb, "datSet") {
								sets = append(sets, ds)
							}
						}
						if len(sets) != 1 {
							g.SchemaError = "missing or ambiguous DataSet"
						} else {
							for _, member := range childrenNamed(sets[0], "FCDA") {
								t, err := resolveGooseMember(b, member, attribute(ld, "inst"), ldByInst, domains)
								if err != nil {
									g.SchemaError = err.Error()
									g.Entries = nil
									break
								}
								g.Entries = append(g.Entries, t)
							}
							remaining := 200000
							for _, t := range g.Entries {
								cost := gooseTypeCost(t, remaining)
								if cost > remaining {
									g.SchemaError = "DataSet exceeds the 200,000 display-node limit"
									g.Entries = nil
									break
								}
								remaining -= cost
							}
						}
						streams = append(streams, g)
					}
				}
			}
		}
	}
	return streams, nil
}

func gooseTypeCost(t *Type, limit int) int {
	cost := 1
	for _, c := range t.Children {
		n := gooseTypeCost(c, limit-cost)
		if t.Kind == "ARRAY" {
			if n < 1 || t.Size > (limit-cost)/n {
				return limit + 1
			}
			n *= t.Size
		}
		cost += n
		if cost > limit {
			return limit + 1
		}
	}
	return cost
}

func sclLNName(x xmlElement) string {
	if x.Name == "LN0" {
		return "LLN0"
	}
	return attribute(x, "prefix") + attribute(x, "lnClass") + attribute(x, "inst")
}

func readGooseAddress(g *GooseStream, root xmlElement, ldInst string) error {
	var matches []xmlElement
	for _, sub := range childrenNamed(childNamed(root, "Communication"), "SubNetwork") {
		for _, ap := range childrenNamed(sub, "ConnectedAP") {
			if attribute(ap, "iedName") != g.IED || attribute(ap, "apName") != g.AccessPoint {
				continue
			}
			for _, x := range childrenNamed(ap, "GSE") {
				if attribute(x, "ldInst") == ldInst && attribute(x, "cbName") == g.Name {
					matches = append(matches, x)
				}
			}
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("missing or ambiguous Communication/GSE address")
	}
	values := map[string]string{}
	for _, p := range childrenNamed(childNamed(matches[0], "Address"), "P") {
		key := attribute(p, "type")
		if _, exists := values[key]; exists {
			return fmt.Errorf("duplicate address parameter %s", key)
		}
		values[key] = strings.TrimSpace(p.Text)
	}
	mac, err := net.ParseMAC(strings.ReplaceAll(values["MAC-Address"], "-", ":"))
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("missing or invalid destination MAC")
	}
	g.DstMAC = mac.String()
	app, err := strconv.ParseUint(values["APPID"], 16, 16)
	if err != nil {
		return fmt.Errorf("missing or invalid numeric APPID")
	}
	g.AppID = uint16(app)
	if raw, ok := values["VLAN-ID"]; ok {
		n, err := strconv.ParseUint(raw, 16, 12)
		if err != nil || n > 4094 {
			return fmt.Errorf("invalid VLAN ID")
		}
		v := uint16(n)
		g.VLANID = &v
	}
	if raw, ok := values["VLAN-PRIORITY"]; ok {
		n, err := strconv.ParseUint(raw, 10, 3)
		if err != nil {
			return fmt.Errorf("invalid VLAN priority")
		}
		v := uint8(n)
		g.VLANPriority = &v
	}
	return nil
}

func resolveGooseMember(b *importBuilder, m xmlElement, current string, devices map[string]xmlElement, domains map[string]string) (*Type, error) {
	inst := attribute(m, "ldInst")
	if inst == "" {
		inst = current
	}
	lnName := attribute(m, "prefix") + attribute(m, "lnClass") + attribute(m, "lnInst")
	fc := attribute(m, "fc")
	if domains[inst] == "" || fc == "" || !IsDataFC(fc) || attribute(m, "ix") != "" {
		return nil, fmt.Errorf("unsupported FCDA domain, FC or array index")
	}
	var nodes []xmlElement
	for _, ln := range devices[inst].Children {
		if (ln.Name == "LN0" || ln.Name == "LN") && sclLNName(ln) == lnName {
			nodes = append(nodes, ln)
		}
	}
	if len(nodes) != 1 {
		return nil, fmt.Errorf("unresolved FCDA logical node %s", lnName)
	}
	definition, err := b.definition(attribute(nodes[0], "lnType"), "LNodeType", 0)
	if err != nil {
		return nil, err
	}
	path := attribute(m, "doName")
	if da := attribute(m, "daName"); da != "" {
		path += "." + da
	}
	parts := strings.Split(path, ".")
	var objects []xmlElement
	for _, obj := range childrenNamed(definition, "DO") {
		if attribute(obj, "name") == parts[0] {
			objects = append(objects, obj)
		}
	}
	if len(objects) != 1 || attribute(objects[0], "count") != "" {
		return nil, fmt.Errorf("unresolved or array FCDA object %s", path)
	}
	groups, err := b.object(parts[0], attribute(objects[0], "type"), 0)
	if err != nil {
		return nil, err
	}
	var value *Type
	for _, group := range groups {
		if group.fc == fc {
			value = group.t
		}
	}
	for _, part := range parts[1:] {
		if value == nil || value.Kind != "STRUCTURE" {
			return nil, fmt.Errorf("unresolved FCDA %s [%s]", path, fc)
		}
		var next *Type
		for _, child := range value.Children {
			if child.Name == part {
				next = child
			}
		}
		value = next
	}
	if value == nil {
		return nil, fmt.Errorf("unresolved FCDA %s [%s]", path, fc)
	}
	copy := *value
	copy.Name = domains[inst] + "/" + lnName + "." + path
	return &copy, nil
}

// ValidatePDU gates labels only. The received packet is never reinterpreted.
func (g *GooseStream) ValidatePDU(p *goose.PDU) error {
	if g.SchemaError != "" {
		return fmt.Errorf("%s", g.SchemaError)
	}
	if g.FrameError != "" {
		return fmt.Errorf("%s", g.FrameError)
	}
	if p.GocbRef != g.GocbRef || p.DatSet != g.DatSet || p.AppID != g.AppID || (g.GoID != "" && p.GoID != g.GoID) {
		return fmt.Errorf("stream identifiers differ from ICD")
	}
	mac, _ := net.ParseMAC(g.DstMAC)
	if !bytes.Equal(mac, p.DstMAC) {
		return fmt.Errorf("destination MAC differs from ICD")
	}
	if g.VLANID != nil && (p.VLAN == nil || p.VLAN.VID != *g.VLANID) {
		return fmt.Errorf("VLAN differs from ICD")
	}
	if g.VLANPriority != nil && (p.VLAN == nil || p.VLAN.Priority != *g.VLANPriority) {
		return fmt.Errorf("VLAN priority differs from ICD")
	}
	if p.ConfRev != g.ConfRev {
		return fmt.Errorf("confRev differs from ICD")
	}
	if len(p.AllData) != len(g.Entries) || p.NumDatSetEntries != uint32(len(g.Entries)) {
		return fmt.Errorf("DataSet entry count differs from ICD")
	}
	for i, t := range g.Entries {
		if !matchesGooseType(t, p.AllData[i]) {
			return fmt.Errorf("entry %d type/structure differs from ICD", i+1)
		}
	}
	return nil
}

func matchesGooseType(t *Type, v goose.DataValue) bool {
	if t.Kind != v.Type.String() {
		return false
	}
	if t.Kind == "STRUCTURE" {
		if len(t.Children) != len(v.Children) {
			return false
		}
		for i, c := range t.Children {
			if !matchesGooseType(c, v.Children[i]) {
				return false
			}
		}
	} else if t.Kind == "ARRAY" {
		if len(t.Children) != 1 || t.Size != len(v.Children) {
			return false
		}
		for _, c := range v.Children {
			if !matchesGooseType(t.Children[0], c) {
				return false
			}
		}
	} else if t.Kind == "BIT_STRING" {
		bits := v.BitLength
		if bits == 0 {
			bits = len(v.Bytes) * 8
		}
		size := t.Size
		if size < 0 {
			size = -size
		}
		if bits != size {
			return false
		}
	}
	return true
}
