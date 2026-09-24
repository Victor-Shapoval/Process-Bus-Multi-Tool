package sclmodel

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

const maxICDSize = 32 << 20

// readSCLRoot parses a bounded SCL document and ignores Private extensions.
func readSCLRoot(data []byte) (root xmlElement, err error) {
	if len(data) > maxICDSize {
		return root, fmt.Errorf("ICD exceeds the 32 MiB limit")
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	budget := 500000
	found := false
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return root, fmt.Errorf("invalid ICD XML: %w", err)
		}
		if start, ok := token.(xml.StartElement); ok {
			if found || start.Name.Local != "SCL" || start.Name.Space != "http://www.iec.ch/61850/2003/SCL" {
				return root, fmt.Errorf("expected one SCL root in the IEC 61850 namespace")
			}
			root, err = readSCLElement(d, start, 0, &budget)
			if err != nil {
				return root, err
			}
			found = true
		} else if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) != "" {
			return root, fmt.Errorf("unexpected text outside SCL root")
		}
	}
	if !found {
		return root, fmt.Errorf("missing SCL root")
	}
	return root, nil
}

// ImportICD reconstructs the readable data tree from standard SCL elements.
// It never restores live readings or consumes PBMT's former Private JSON.
// One project represents one IED and one MMS server access point.
func ImportICD(data []byte) (*Model, error) {
	root, err := readSCLRoot(data)
	if err != nil {
		return nil, err
	}
	ieds := childrenNamed(root, "IED")
	if len(ieds) != 1 {
		return nil, fmt.Errorf("ICD must describe exactly one IED (found %d)", len(ieds))
	}
	ied := ieds[0]
	var aps []xmlElement
	for _, ap := range childrenNamed(ied, "AccessPoint") {
		if len(childrenNamed(ap, "Server")) == 1 {
			aps = append(aps, ap)
		}
	}
	if len(aps) != 1 {
		return nil, fmt.Errorf("ICD must contain one server access point (found %d)", len(aps))
	}
	ap := aps[0]
	m := &Model{Endpoint: Endpoint{Port: 102}}
	var connection xmlElement
	for _, sub := range childrenNamed(childNamed(root, "Communication"), "SubNetwork") {
		for _, cap := range childrenNamed(sub, "ConnectedAP") {
			if attribute(cap, "iedName") == attribute(ied, "name") && attribute(cap, "apName") == attribute(ap, "name") {
				if connection.Name != "" {
					return nil, fmt.Errorf("multiple communication addresses for the IED access point")
				}
				connection = cap
			}
		}
	}
	for _, p := range childrenNamed(childNamed(connection, "Address"), "P") {
		if attribute(p, "type") == "IP" {
			ip := net.ParseIP(strings.TrimSpace(p.Text))
			if ip == nil {
				return nil, fmt.Errorf("invalid IED IP address in ICD")
			}
			m.Endpoint.IP = ip.String()
		}
	}
	b := &importBuilder{types: map[string]xmlElement{}, budget: 200000}
	for _, typ := range childNamed(root, "DataTypeTemplates").Children {
		id := attribute(typ, "id")
		if id == "" || b.types[id].Name != "" {
			return nil, fmt.Errorf("missing or duplicate type ID %q", id)
		}
		b.types[id] = typ
	}
	devices := childrenNamed(childNamed(ap, "Server"), "LDevice")
	domains := map[string]string{}
	seenDomains := map[string]bool{}
	for _, ld := range devices {
		inst := attribute(ld, "inst")
		domain := attribute(ld, "ldName")
		if domain == "" {
			domain = attribute(ied, "name") + inst
		}
		if inst == "" || domains[inst] != "" || seenDomains[domain] {
			return nil, fmt.Errorf("missing or duplicate logical device %q", inst)
		}
		domains[inst], seenDomains[domain] = domain, true
	}
	for _, ld := range devices {
		device := LogicalDevice{Name: domains[attribute(ld, "inst")]}
		seen := map[string]bool{}
		for _, node := range ld.Children {
			if node.Name != "LN0" && node.Name != "LN" {
				continue
			}
			name := attribute(node, "prefix") + attribute(node, "lnClass") + attribute(node, "inst")
			if node.Name == "LN0" {
				name = "LLN0"
			}
			if _, _, _, err := splitLN(name); err != nil || seen[name] {
				return nil, fmt.Errorf("invalid or duplicate logical node %q", name)
			}
			seen[name] = true
			ln, err := b.node(node, name, device.Name, domains, connection, attribute(ld, "inst"))
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", device.Name, name, err)
			}
			device.Nodes = append(device.Nodes, ln)
		}
		if !seen["LLN0"] {
			return nil, fmt.Errorf("%s has no LLN0", device.Name)
		}
		m.Devices = append(m.Devices, device)
	}
	if len(m.Devices) == 0 {
		return nil, fmt.Errorf("ICD contains no logical devices")
	}
	return m, nil
}

func readSCLElement(d *xml.Decoder, start xml.StartElement, depth int, budget *int) (xmlElement, error) {
	*budget -= 1
	if depth > 64 || *budget < 0 {
		return xmlElement{}, fmt.Errorf("ICD exceeds XML depth or element limits")
	}
	x := xmlElement{Name: start.Name.Local, Attr: start.Attr}
	for {
		token, err := d.Token()
		if err != nil {
			return x, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Local == "Private" || token.Name.Space != start.Name.Space {
				if err := d.Skip(); err != nil {
					return x, err
				}
				continue
			}
			child, err := readSCLElement(d, token, depth+1, budget)
			if err != nil {
				return x, err
			}
			x.Children = append(x.Children, child)
		case xml.CharData:
			x.Text += string(token)
		case xml.EndElement:
			return x, nil
		}
	}
}

func childrenNamed(x xmlElement, name string) []xmlElement {
	var result []xmlElement
	for _, child := range x.Children {
		if child.Name == name {
			result = append(result, child)
		}
	}
	return result
}

func childNamed(x xmlElement, name string) xmlElement {
	for _, child := range x.Children {
		if child.Name == name {
			return child
		}
	}
	return xmlElement{}
}

type importBuilder struct {
	types  map[string]xmlElement
	budget int
}

func (b *importBuilder) definition(id, kind string, depth int) (xmlElement, error) {
	b.budget--
	x := b.types[id]
	if depth > 48 || b.budget < 0 {
		return x, fmt.Errorf("cyclic or oversized SCL type graph")
	}
	if x.Name != kind {
		return x, fmt.Errorf("unresolved %s %q", kind, id)
	}
	return x, nil
}

func (b *importBuilder) object(name, id string, depth int) ([]fcType, error) {
	x, err := b.definition(id, "DOType", depth)
	if err != nil {
		return nil, err
	}
	var result []fcType
	add := func(fc string, attr *Type) {
		for _, group := range result {
			if group.fc == fc {
				group.t.Children = append(group.t.Children, attr)
				return
			}
		}
		result = append(result, fcType{fc: fc, t: &Type{Name: name, Kind: "STRUCTURE", Children: []*Type{attr}}})
	}
	seen := map[string]bool{}
	for _, a := range x.Children {
		if a.Name != "DA" && a.Name != "SDO" {
			continue
		}
		name := attribute(a, "name")
		if name == "" || seen[name] {
			return nil, fmt.Errorf("missing or duplicate data attribute %q", name)
		}
		seen[name] = true
		if a.Name == "SDO" {
			if attribute(a, "count") != "" {
				return nil, fmt.Errorf("SDO arrays are not supported: %s", name)
			}
			parts, err := b.object(name, attribute(a, "type"), depth+1)
			if err != nil {
				return nil, err
			}
			for _, p := range parts {
				add(p.fc, p.t)
			}
			continue
		}
		fc := attribute(a, "fc")
		if !IsDataFC(fc) || fc == "" {
			return nil, fmt.Errorf("unsupported functional constraint %q", fc)
		}
		t, err := b.dataAttribute(a, depth+1)
		if err != nil {
			return nil, err
		}
		add(fc, t)
	}
	return result, nil
}

func (b *importBuilder) dataAttribute(a xmlElement, depth int) (*Type, error) {
	b.budget--
	if depth > 48 || b.budget < 0 {
		return nil, fmt.Errorf("cyclic or oversized SCL attribute graph")
	}
	t := &Type{Name: attribute(a, "name")}
	basic := attribute(a, "bType")
	switch basic {
	case "Struct":
		x, err := b.definition(attribute(a, "type"), "DAType", depth)
		if err != nil {
			return nil, err
		}
		t.Kind = "STRUCTURE"
		seen := map[string]bool{}
		for _, child := range childrenNamed(x, "BDA") {
			name := attribute(child, "name")
			if name == "" || seen[name] {
				return nil, fmt.Errorf("missing or duplicate BDA %q", name)
			}
			seen[name] = true
			v, err := b.dataAttribute(child, depth+1)
			if err != nil {
				return nil, err
			}
			t.Children = append(t.Children, v)
		}
	case "BOOLEAN":
		t.Kind = "BOOLEAN"
	case "Timestamp":
		t.Kind = "UTC_TIME"
	case "EntryTime":
		t.Kind, t.Size = "BINARY_TIME", 6
	case "ObjRef":
		t.Kind, t.Size = "VISIBLE_STRING", 129
	case "Unicode255":
		t.Kind, t.Size = "MMS_STRING", 255
	case "EntryID":
		t.Kind, t.Size = "OCTET_STRING", 8
	case "Quality":
		t.Kind, t.Size = "BIT_STRING", 13
	case "Check", "Dbpos":
		t.Kind, t.Size = "BIT_STRING", 2
	case "TrgOps":
		t.Kind, t.Size = "BIT_STRING", 6
	case "OptFlds":
		t.Kind, t.Size = "BIT_STRING", 10
	default:
		allowed := " INT8 INT16 INT24 INT32 INT64 INT128 INT8U INT16U INT24U INT32U FLOAT32 FLOAT64 VisString32 VisString64 VisString65 VisString129 VisString255 Octet6 Octet16 Octet64 "
		if basic == "" || strings.ContainsAny(basic, " \t\r\n") || !strings.Contains(allowed, " "+basic+" ") {
			return nil, fmt.Errorf("unsupported SCL basic type %q", basic)
		}
		for _, prefix := range []string{"INT", "FLOAT", "VisString", "Octet"} {
			if !strings.HasPrefix(basic, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(basic, prefix)
			t.Kind = map[string]string{"INT": "INTEGER", "FLOAT": "FLOAT", "VisString": "VISIBLE_STRING", "Octet": "OCTET_STRING"}[prefix]
			if prefix == "INT" && strings.HasSuffix(suffix, "U") {
				t.Kind, suffix = "UNSIGNED", strings.TrimSuffix(suffix, "U")
			}
			size, err := strconv.Atoi(suffix)
			if err != nil || size < 1 || size > 255 {
				return nil, fmt.Errorf("invalid SCL basic type %q", basic)
			}
			t.Size = size
			break
		}
		if t.Kind == "" {
			return nil, fmt.Errorf("unsupported SCL basic type %q", basic)
		}
	}
	if count := attribute(a, "count"); count != "" {
		n, err := strconv.Atoi(count)
		if err != nil || n < 1 || n > 200000 {
			return nil, fmt.Errorf("invalid array count %q", count)
		}
		t = &Type{Name: t.Name, Kind: "ARRAY", Size: n, Children: []*Type{t}}
	}
	return t, nil
}

func (b *importBuilder) node(x xmlElement, name, domain string, domains map[string]string, connection xmlElement, ldInst string) (LogicalNode, error) {
	ln := LogicalNode{Name: name}
	typ, err := b.definition(attribute(x, "lnType"), "LNodeType", 0)
	if err != nil {
		return ln, err
	}
	seen := map[string]bool{}
	for _, obj := range childrenNamed(typ, "DO") {
		name := attribute(obj, "name")
		if name == "" || seen[name] || attribute(obj, "count") != "" {
			return ln, fmt.Errorf("invalid, duplicate or array DO %q", name)
		}
		seen[name] = true
		parts, err := b.object(name, attribute(obj, "type"), 0)
		if err != nil {
			return ln, err
		}
		for _, part := range parts {
			var group *Type
			for _, g := range ln.Groups {
				if g.Name == part.fc {
					group = g
				}
			}
			if group == nil {
				group = &Type{Name: part.fc, Kind: "STRUCTURE"}
				ln.Groups = append(ln.Groups, group)
			}
			group.Children = append(group.Children, part.t)
		}
	}
	for _, ds := range childrenNamed(x, "DataSet") {
		set := DataSet{Name: attribute(ds, "name")}
		for _, member := range childrenNamed(ds, "FCDA") {
			inst := attribute(member, "ldInst")
			if inst == "" {
				inst = ldInst
			}
			ld := domains[inst]
			fc, obj := attribute(member, "fc"), attribute(member, "doName")
			if ld == "" || !IsDataFC(fc) || fc == "" || obj == "" || attribute(member, "ix") != "" {
				return ln, fmt.Errorf("unsupported DataSet member in %s", set.Name)
			}
			ref := ld + "/" + attribute(member, "prefix") + attribute(member, "lnClass") + attribute(member, "lnInst") + "." + obj
			if da := attribute(member, "daName"); da != "" {
				ref += "." + da
			}
			set.Members = append(set.Members, ref+"["+fc+"]")
		}
		ln.DataSets = append(ln.DataSets, set)
	}
	if err := importControls(&ln, x, domain, connection, ldInst); err != nil {
		return ln, err
	}
	return ln, nil
}

// Control settings remain configuration snapshots, not fabricated MMS schemas.
func importControls(ln *LogicalNode, x xmlElement, domain string, connection xmlElement, ldInst string) error {
	var parseErr error
	number := func(x xmlElement, name string, base int) uint32 {
		s := attribute(x, name)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseUint(s, base, 32)
		if err != nil {
			parseErr = fmt.Errorf("invalid %s: %q", name, s)
		}
		return uint32(n)
	}
	flag := func(x xmlElement, name string) bool {
		s := attribute(x, name)
		if s == "" {
			return false
		}
		v, err := strconv.ParseBool(s)
		if err != nil {
			parseErr = fmt.Errorf("invalid %s: %q", name, s)
		}
		return v
	}
	dataset := func(x xmlElement) string {
		if name := attribute(x, "datSet"); name != "" {
			return domain + "/" + ln.Name + "." + name
		}
		return ""
	}
	for _, r := range childrenNamed(x, "ReportControl") {
		// Indexed instance names are not inferable from a base control name.
		if attribute(r, "indexed") != "false" && attribute(r, "indexed") != "0" {
			return fmt.Errorf("indexed report controls require Read Model")
		}
		trg, opt := childNamed(r, "TrgOps"), childNamed(r, "OptFields")
		ln.Reports = append(ln.Reports, ReportControl{
			Name: attribute(r, "name"), ReportID: attribute(r, "rptID"), DataSet: dataset(r), Buffered: flag(r, "buffered"),
			ConfRev: number(r, "confRev", 10), BufferTime: number(r, "bufTime", 10), IntegrityPeriod: number(r, "intgPd", 10),
			Triggers: ReportTriggers{DataChange: flag(trg, "dchg"), QualityChange: flag(trg, "qchg"), DataUpdate: flag(trg, "dupd"), Integrity: flag(trg, "period"), GI: flag(trg, "gi")},
			Options:  ReportOptions{SeqNum: flag(opt, "seqNum"), TimeStamp: flag(opt, "timeStamp"), ReasonCode: flag(opt, "reasonCode"), DataSet: flag(opt, "dataSet"), DataRef: flag(opt, "dataRef"), BufOvfl: flag(opt, "bufOvfl"), EntryID: flag(opt, "entryID"), ConfRev: flag(opt, "configRef")},
		})
	}
	if sg := childNamed(x, "SettingControl"); sg.Name != "" {
		ln.Settings = &SettingControl{NumGroups: number(sg, "numOfSGs", 10), ActiveGroup: number(sg, "actSG", 10)}
		if ln.Settings.NumGroups == 0 || ln.Settings.ActiveGroup == 0 || ln.Settings.ActiveGroup > ln.Settings.NumGroups {
			return fmt.Errorf("invalid setting group parameters")
		}
	}
	for _, cb := range childrenNamed(x, "GSEControl") {
		g := GooseControl{Name: attribute(cb, "name"), DataSet: dataset(cb), GoID: attribute(cb, "appID"), ConfRev: number(cb, "confRev", 10)}
		for _, comm := range childrenNamed(connection, "GSE") {
			if attribute(comm, "ldInst") != ldInst || attribute(comm, "cbName") != g.Name {
				continue
			}
			for _, p := range childrenNamed(childNamed(comm, "Address"), "P") {
				v := element("value", "value", strings.TrimSpace(p.Text))
				switch attribute(p, "type") {
				case "MAC-Address":
					g.MAC = strings.TrimSpace(p.Text)
				case "APPID":
					g.APPID = int(number(v, "value", 16))
				case "VLAN-ID":
					g.VLAN = int(number(v, "value", 16))
				case "VLAN-PRIORITY":
					g.Priority = int(number(v, "value", 10))
				}
			}
			for _, spec := range []struct {
				name string
				out  *uint32
			}{{"MinTime", &g.MinTime}, {"MaxTime", &g.MaxTime}} {
				tm := childNamed(comm, spec.name)
				if tm.Name == "" {
					continue
				}
				if attribute(tm, "unit") != "s" || attribute(tm, "multiplier") != "m" {
					return fmt.Errorf("unsupported %s units", spec.name)
				}
				*spec.out = number(element("value", "value", strings.TrimSpace(tm.Text)), "value", 10)
			}
		}
		ln.GOOSE = append(ln.GOOSE, g)
	}
	return parseErr
}
