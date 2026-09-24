package sclmodel

import (
	"encoding/xml"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Online discovery cannot recover CDCs or enumeration labels unambiguously.
// Explicitly report objects that cannot be represented in standard SCL.
type xmlElement struct {
	Name     string
	Attr     []xml.Attr
	Children []xmlElement
	Text     string
}

func element(name string, pairs ...string) xmlElement {
	x := xmlElement{Name: name}
	for i := 0; i+1 < len(pairs); i += 2 {
		x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: pairs[i]}, Value: pairs[i+1]})
	}
	return x
}
func (x xmlElement) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{Name: xml.Name{Local: x.Name}, Attr: x.Attr}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if x.Text != "" {
		if err := e.EncodeToken(xml.CharData(x.Text)); err != nil {
			return err
		}
	}
	for _, ch := range x.Children {
		if err := e.Encode(ch); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

var lnPattern = regexp.MustCompile(`^(.*?)([A-Z]{4})([0-9]*)$`)

func splitLN(name string) (prefix, class, inst string, err error) {
	if name == "LLN0" {
		return "", "LLN0", "", nil
	}
	m := lnPattern.FindStringSubmatch(name)
	if m == nil || m[3] == "" {
		return "", "", "", fmt.Errorf("cannot reconstruct logical node name %q", name)
	}
	return m[1], m[2], m[3], nil
}

type exportBuilder struct {
	lns, dos, das []xmlElement
	warnings      []string
	serial        int
}

func (b *exportBuilder) id() string { b.serial++; return fmt.Sprintf("PBMT_T%d", b.serial) }

func ExportICD(model *Model) ([]byte, []string, error) {
	if model == nil || len(model.Devices) == 0 {
		return nil, nil, fmt.Errorf("read a model before exporting")
	}
	b := &exportBuilder{warnings: append([]string{}, model.Warnings...)}
	b.warnings = append(b.warnings, "Online reconstruction: CDCs are inferred from structure; enum labels and engineering descriptions are unavailable. Objects/control blocks that cannot be represented in SCL are omitted.")
	root := element("SCL", "xmlns", "http://www.iec.ch/61850/2003/SCL", "version", "2007", "revision", "B", "release", "4")
	header := element("Header", "id", "PBMT_Discovered", "toolID", "PBMT", "nameStructure", "IEDName")
	root.Children = append(root.Children, header)
	const iedName = "DiscoveredIED"
	communication := element("Communication")
	sub := element("SubNetwork", "name", "MMS", "type", "8-MMS")
	cap := element("ConnectedAP", "iedName", iedName, "apName", "AP1")
	address := element("Address")
	ip := element("P", "type", "IP")
	ip.Text = model.Endpoint.IP
	address.Children = append(address.Children, ip)
	cap.Children = append(cap.Children, address)
	ied := element("IED", "name", iedName)
	ap := element("AccessPoint", "name", "AP1")
	server := element("Server")
	server.Children = append(server.Children, element("Authentication"))
	ldMap := map[string]string{}
	for i, ld := range model.Devices {
		ldMap[ld.Name] = fmt.Sprintf("LD%d", i+1)
	}
	for _, ld := range model.Devices {
		ldx := element("LDevice", "inst", ldMap[ld.Name], "ldName", ld.Name)
		nodes := append([]LogicalNode{}, ld.Nodes...)
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name == "LLN0" && nodes[j].Name != "LLN0" })
		hasLN0 := false
		for _, ln := range nodes {
			prefix, class, inst, err := splitLN(ln.Name)
			if err != nil {
				return nil, b.warnings, err
			}
			typeID := b.id()
			lnType := element("LNodeType", "id", typeID, "lnClass", class)
			tag := "LN"
			if ln.Name == "LLN0" {
				tag = "LN0"
				hasLN0 = true
			}
			lnx := element(tag, "lnClass", class, "inst", inst, "lnType", typeID)
			if prefix != "" {
				lnx.Attr = append(lnx.Attr, xml.Attr{Name: xml.Name{Local: "prefix"}, Value: prefix})
			}
			objects := map[string][]fcType{}
			var order []string
			for _, group := range ln.Groups {
				if !IsDataFC(group.Name) {
					if group.Name != "GO" && group.Name != "RP" && group.Name != "BR" {
						b.warnings = append(b.warnings, ld.Name+"/"+ln.Name+" ["+group.Name+"]: unsupported control metadata omitted")
					}
					continue
				}
				for _, obj := range group.Children {
					if ln.Name == "LLN0" && group.Name == "SP" && obj.Name == "SGCB" {
						continue
					}
					if _, ok := objects[obj.Name]; !ok {
						order = append(order, obj.Name)
					}
					objects[obj.Name] = append(objects[obj.Name], fcType{group.Name, obj})
				}
			}
			for _, name := range order {
				// Failed objects must not leave orphaned partial type definitions.
				doCount, daCount := len(b.dos), len(b.das)
				id, err := b.object(objects[name])
				if err != nil {
					b.dos, b.das = b.dos[:doCount], b.das[:daCount]
					b.warnings = append(b.warnings, ld.Name+"/"+ln.Name+"."+name+": "+err.Error()+"; omitted")
					continue
				}
				lnType.Children = append(lnType.Children, element("DO", "name", name, "type", id))
			}
			b.lns = append(b.lns, lnType)
			for _, ds := range ln.DataSets {
				dsx := element("DataSet", "name", ds.Name)
				valid := true
				for _, member := range ds.Members {
					fcda, err := exportMember(member, ldMap)
					if err != nil {
						valid = false
						b.warnings = append(b.warnings, ds.Name+": "+err.Error())
						break
					}
					dsx.Children = append(dsx.Children, fcda)
				}
				if valid {
					lnx.Children = append(lnx.Children, dsx)
				}
			}
			for _, cb := range ln.GOOSE {
				if ln.Name != "LLN0" {
					b.warnings = append(b.warnings, "GoCB outside LLN0 omitted: "+cb.Name)
					continue
				}
				ds := strings.ReplaceAll(cb.DataSet, "$", ".")
				expected := ld.Name + "/" + ln.Name + "."
				if !strings.HasPrefix(ds, expected) {
					b.warnings = append(b.warnings, "External GoCB DataSet omitted: "+cb.Name)
					continue
				}
				lnx.Children = append(lnx.Children, element("GSEControl", "name", cb.Name, "type", "GOOSE", "appID", cb.GoID, "datSet", strings.TrimPrefix(ds, expected), "confRev", strconv.FormatUint(uint64(cb.ConfRev), 10)))
				gse := element("GSE", "ldInst", ldMap[ld.Name], "cbName", cb.Name)
				addr := element("Address")
				for _, pair := range [][2]string{{"MAC-Address", cb.MAC}, {"APPID", fmt.Sprintf("%04X", cb.APPID)}, {"VLAN-ID", fmt.Sprintf("%03X", cb.VLAN)}, {"VLAN-PRIORITY", strconv.Itoa(cb.Priority)}} {
					p := element("P", "type", pair[0])
					p.Text = pair[1]
					addr.Children = append(addr.Children, p)
				}
				gse.Children = append(gse.Children, addr)
				for _, v := range []struct {
					name  string
					value uint32
				}{{"MinTime", cb.MinTime}, {"MaxTime", cb.MaxTime}} {
					if v.value > 0 {
						e := element(v.name, "unit", "s", "multiplier", "m")
						e.Text = strconv.FormatUint(uint64(v.value), 10)
						gse.Children = append(gse.Children, e)
					}
				}
				cap.Children = append(cap.Children, gse)
			}
			b.appendControlElements(&lnx, ld.Name, ln)
			ldx.Children = append(ldx.Children, lnx)
		}
		if !hasLN0 {
			return nil, b.warnings, fmt.Errorf("%s has no LLN0; cannot export an ICD", ld.Name)
		}
		server.Children = append(server.Children, ldx)
	}
	b.reconcileDataSets(&server, &cap)
	sub.Children = append(sub.Children, cap)
	communication.Children = append(communication.Children, sub)
	ap.Children = append(ap.Children, server)
	ied.Children = append(ied.Children, ap)
	types := element("DataTypeTemplates")
	types.Children = append(types.Children, b.lns...)
	types.Children = append(types.Children, b.dos...)
	types.Children = append(types.Children, b.das...)
	root.Children = append(root.Children, communication, ied, types)
	data, err := xml.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, b.warnings, err
	}
	return append([]byte(xml.Header), append(data, '\n')...), b.warnings, nil
}

type fcType struct {
	fc string
	t  *Type
}

func (b *exportBuilder) appendControlElements(lnx *xmlElement, domain string, ln LogicalNode) {
	available := map[string]bool{}
	for _, r := range ln.Reports {
		fc := "RP"
		if r.Buffered {
			fc = "BR"
		}
		available[fc+"/"+r.Name] = true
		ref := strings.ReplaceAll(r.DataSet, "$", ".")
		prefix := domain + "/" + ln.Name + "."
		if ref != "" && !strings.HasPrefix(ref, prefix) {
			b.warnings = append(b.warnings, domain+"/"+ln.Name+"."+r.Name+": external report DataSet omitted")
			continue
		}
		// Each discovered instance has an exact MMS name; do not invent indexed instances.
		x := element("ReportControl", "name", r.Name, "rptID", r.ReportID, "buffered", strconv.FormatBool(r.Buffered), "indexed", "false", "confRev", strconv.FormatUint(uint64(r.ConfRev), 10), "bufTime", strconv.FormatUint(uint64(r.BufferTime), 10), "intgPd", strconv.FormatUint(uint64(r.IntegrityPeriod), 10))
		if ref != "" {
			setAttribute(&x, "datSet", strings.TrimPrefix(ref, prefix))
		}
		t := r.Triggers
		o := r.Options
		x.Children = append(x.Children, element("TrgOps", "dchg", strconv.FormatBool(t.DataChange), "qchg", strconv.FormatBool(t.QualityChange), "dupd", strconv.FormatBool(t.DataUpdate), "period", strconv.FormatBool(t.Integrity), "gi", strconv.FormatBool(t.GI)))
		x.Children = append(x.Children, element("OptFields", "seqNum", strconv.FormatBool(o.SeqNum), "timeStamp", strconv.FormatBool(o.TimeStamp), "reasonCode", strconv.FormatBool(o.ReasonCode), "dataSet", strconv.FormatBool(o.DataSet), "dataRef", strconv.FormatBool(o.DataRef), "bufOvfl", strconv.FormatBool(o.BufOvfl), "entryID", strconv.FormatBool(o.EntryID), "configRef", strconv.FormatBool(o.ConfRev)))
		lnx.Children = append(lnx.Children, x)
	}
	for _, g := range ln.Groups {
		if g.Name == "RP" || g.Name == "BR" {
			for _, r := range g.Children {
				if !available[g.Name+"/"+r.Name] {
					b.warnings = append(b.warnings, domain+"/"+ln.Name+"."+r.Name+": report configuration values unavailable; run Read Model again")
				}
			}
		}
		if ln.Name == "LLN0" && g.Name == "SP" {
			for _, obj := range g.Children {
				if obj.Name == "SGCB" && ln.Settings == nil {
					b.warnings = append(b.warnings, domain+"/LLN0.SGCB: setting group values unavailable; run Read Model again")
				}
			}
		}
	}
	if s := ln.Settings; s != nil {
		if ln.Name != "LLN0" || s.NumGroups == 0 || s.ActiveGroup == 0 || s.ActiveGroup > s.NumGroups {
			b.warnings = append(b.warnings, domain+"/"+ln.Name+": invalid setting group parameters; omitted")
		} else {
			lnx.Children = append(lnx.Children, element("SettingControl", "numOfSGs", strconv.FormatUint(uint64(s.NumGroups), 10), "actSG", strconv.FormatUint(uint64(s.ActiveGroup), 10)))
		}
	}
	// LN0 extends the common LN sequence: reports precede GSE and settings.
	rank := map[string]int{"DataSet": 0, "ReportControl": 1, "GSEControl": 2, "SettingControl": 3}
	sort.SliceStable(lnx.Children, func(i, j int) bool { return rank[lnx.Children[i].Name] < rank[lnx.Children[j].Name] })
}

func attribute(x xmlElement, name string) string {
	for _, a := range x.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func setAttribute(x *xmlElement, name, value string) {
	for i := range x.Attr {
		if x.Attr[i].Name.Local == name {
			x.Attr[i].Value = value
			return
		}
	}
	x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: name}, Value: value})
}

// Never export a DataSet with a changed order or a dangling member. The entire
// unresolved DataSet is omitted, along with its dependent control blocks.
func (b *exportBuilder) reconcileDataSets(server, cap *xmlElement) {
	types := map[string]xmlElement{}
	for _, group := range [][]xmlElement{b.lns, b.dos, b.das} {
		for _, t := range group {
			types[attribute(t, "id")] = t
		}
	}
	nodes := map[string]xmlElement{}
	for _, ld := range server.Children {
		if ld.Name != "LDevice" {
			continue
		}
		for _, ln := range ld.Children {
			nodes[attribute(ld, "inst")+"/"+attribute(ln, "prefix")+attribute(ln, "lnClass")+attribute(ln, "inst")] = types[attribute(ln, "lnType")]
		}
	}
	resolve := func(member *xmlElement) bool {
		key := attribute(*member, "ldInst") + "/" + attribute(*member, "prefix") + attribute(*member, "lnClass") + attribute(*member, "lnInst")
		current, ok := nodes[key]
		if !ok {
			return false
		}
		parts := strings.Split(attribute(*member, "doName"), ".")
		if da := attribute(*member, "daName"); da != "" {
			parts = append(parts, strings.Split(da, ".")...)
		}
		var objects, attrs []string
		for _, name := range parts {
			found := false
			for _, child := range current.Children {
				if attribute(child, "name") != name {
					continue
				}
				found = true
				switch child.Name {
				case "DO", "SDO":
					objects = append(objects, name)
				case "DA":
					if attribute(child, "fc") != attribute(*member, "fc") {
						return false
					}
					attrs = append(attrs, name)
				case "BDA":
					attrs = append(attrs, name)
				default:
					return false
				}
				current = types[attribute(child, "type")]
				break
			}
			if !found {
				return false
			}
		}
		setAttribute(member, "doName", strings.Join(objects, "."))
		if len(attrs) > 0 {
			setAttribute(member, "daName", strings.Join(attrs, "."))
		} else {
			filtered := member.Attr[:0]
			for _, a := range member.Attr {
				if a.Name.Local != "daName" {
					filtered = append(filtered, a)
				}
			}
			member.Attr = filtered
		}
		return true
	}
	validControls := map[string]bool{}
	for di := range server.Children {
		ld := &server.Children[di]
		if ld.Name != "LDevice" {
			continue
		}
		for ni := range ld.Children {
			ln := &ld.Children[ni]
			var children []xmlElement
			sets := map[string]bool{}
			for _, child := range ln.Children {
				if child.Name != "DataSet" {
					continue
				}
				valid := len(child.Children) > 0
				for mi := range child.Children {
					if !resolve(&child.Children[mi]) {
						valid = false
					}
				}
				if valid {
					children = append(children, child)
					sets[attribute(child, "name")] = true
				} else {
					b.warnings = append(b.warnings, "DataSet "+attribute(child, "name")+": unresolved member; whole DataSet omitted")
				}
			}
			for _, child := range ln.Children {
				if child.Name == "DataSet" {
					continue
				}
				if child.Name == "GSEControl" && !sets[attribute(child, "datSet")] {
					b.warnings = append(b.warnings, "GoCB "+attribute(child, "name")+": DataSet unavailable; omitted")
					continue
				}
				if child.Name == "ReportControl" && attribute(child, "datSet") != "" && !sets[attribute(child, "datSet")] {
					b.warnings = append(b.warnings, "ReportControl "+attribute(child, "name")+": DataSet unavailable; omitted")
					continue
				}
				if child.Name == "GSEControl" {
					validControls[attribute(*ld, "inst")+"/"+attribute(child, "name")] = true
				}
				children = append(children, child)
			}
			ln.Children = children
		}
	}
	var communication []xmlElement
	for _, child := range cap.Children {
		if child.Name != "GSE" || validControls[attribute(child, "ldInst")+"/"+attribute(child, "cbName")] {
			communication = append(communication, child)
		}
	}
	cap.Children = communication
}

func (b *exportBuilder) object(groups []fcType) (string, error) {
	attrs := map[string][]fcType{}
	var order []string
	for _, g := range groups {
		if g.t.Kind != "STRUCTURE" {
			return "", fmt.Errorf("non-structure data object")
		}
		for _, t := range g.t.Children {
			if _, ok := attrs[t.Name]; !ok {
				order = append(order, t.Name)
			}
			attrs[t.Name] = append(attrs[t.Name], fcType{g.fc, t})
		}
	}
	cdc := inferCDC(attrs)
	if cdc == "" {
		return "", fmt.Errorf("CDC is not recoverable")
	}
	id := b.id()
	obj := element("DOType", "id", id, "cdc", cdc)
	for _, name := range order {
		values := attrs[name]
		v := values[0]
		// An SDO can contain MX measurements, CF settings and DC descriptions.
		// Merge these partitions before validating the FC of individual DAs.
		subObject := false
		for _, part := range values {
			if isSubObject(part.t) {
				subObject = true
			}
		}
		if subObject {
			typeID, err := b.object(values)
			if err != nil {
				return "", err
			}
			obj.Children = append(obj.Children, element("SDO", "name", name, "type", typeID))
			continue
		}
		// SG and SE describe the same setting attribute in SCL.
		for _, other := range values[1:] {
			settingPair := (v.fc == "SE" && other.fc == "SG") || (v.fc == "SG" && other.fc == "SE")
			if !settingPair || !reflect.DeepEqual(v.t, other.t) {
				return "", fmt.Errorf("ambiguous FC for %s", name)
			}
			if other.fc == "SG" {
				v = other
			}
		}
		da, err := b.attribute(v.t, "DA")
		if err != nil {
			return "", err
		}
		da.Attr = append(da.Attr, xml.Attr{Name: xml.Name{Local: "fc"}, Value: v.fc})
		obj.Children = append(obj.Children, da)
	}
	b.dos = append(b.dos, obj)
	return id, nil
}

func isSubObject(t *Type) bool {
	if t.Kind != "STRUCTURE" {
		return false
	}
	switch t.Name {
	case "Oper", "SBOw", "Cancel", "mag", "cVal", "instMag", "subMag", "subCVal", "rangeC", "units":
		return false
	}
	for _, c := range t.Children {
		if c.Name == "q" || c.Name == "t" {
			return true
		}
	}
	return false
}

func inferCDC(attrs map[string][]fcType) string {
	get := func(n string) *Type {
		if a := attrs[n]; len(a) > 0 {
			return a[0].t
		}
		return nil
	}
	if ref := get("setSrcRef"); ref != nil && ref.Kind == "VISIBLE_STRING" && (ref.Size == 129 || ref.Size == -129) {
		return "ORG"
	}
	if tm := get("setTm"); tm != nil && tm.Kind == "UTC_TIME" {
		return "TSG"
	}
	if get("objRef") != nil && get("serviceType") != nil && get("errorCode") != nil {
		if get("ctlVal") != nil && get("origin") != nil {
			return "CTS"
		}
		if get("rptID") != nil {
			if get("purgeBuf") != nil && get("entryID") != nil {
				return "BTS"
			}
			if get("resv") != nil {
				return "UTS"
			}
		}
		if get("numOfSG") != nil && get("actSG") != nil {
			return "STS"
		}
		common := map[string]bool{"objRef": true, "serviceType": true, "errorCode": true, "t": true, "d": true, "dU": true, "cdcNs": true, "cdcName": true, "dataNs": true}
		for name := range attrs {
			if !common[name] {
				return ""
			}
		}
		return "CST"
	}
	if get("phsA") != nil && get("phsB") != nil && get("phsC") != nil {
		return "WYE"
	}
	if get("phsAB") != nil && get("phsBC") != nil {
		return "DEL"
	}
	if get("c1") != nil && get("c2") != nil {
		return "SEQ"
	}
	if get("vendor") != nil {
		if get("ldNs") != nil || get("lnNs") != nil || get("configRev") != nil {
			return "LPL"
		}
		return "DPL"
	}
	if get("general") != nil {
		if get("dirGeneral") != nil {
			return "ACD"
		}
		return "ACT"
	}
	if get("cVal") != nil {
		return "CMV"
	}
	if get("mag") != nil {
		return "MV"
	}
	if get("instMag") != nil {
		return "SAV"
	}
	if get("actVal") != nil {
		return "BCR"
	}
	if t := get("stVal"); t != nil {
		control := get("Oper") != nil || get("ctlModel") != nil
		switch t.Kind {
		case "BOOLEAN":
			if control {
				return "SPC"
			}
			return "SPS"
		case "BIT_STRING":
			if t.Size == 2 || t.Size == -2 {
				if control {
					return "DPC"
				}
				return "DPS"
			}
		case "INTEGER":
			if control {
				return "INC"
			}
			return "INS"
		case "VISIBLE_STRING":
			if t.Size == 255 || t.Size == -255 {
				return "VSS"
			}
		}
	}
	if t := get("setVal"); t != nil {
		switch t.Kind {
		case "BOOLEAN":
			return "SPG"
		case "INTEGER":
			return "ING"
		case "VISIBLE_STRING":
			return "VSG"
		case "STRUCTURE":
			return "ASG"
		}
	}
	if get("setMag") != nil {
		return "ASG"
	}
	return ""
}

func (b *exportBuilder) attribute(t *Type, tag string) (xmlElement, error) {
	x := element(tag, "name", t.Name)
	base := t
	if t.Kind == "ARRAY" {
		if len(t.Children) != 1 || t.Size < 1 {
			return x, fmt.Errorf("invalid array %s", t.Name)
		}
		base = t.Children[0]
		x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: "count"}, Value: strconv.Itoa(t.Size)})
	}
	basic := ""
	typeID := ""
	switch base.Kind {
	case "STRUCTURE":
		basic = "Struct"
		typeID = b.id()
		typ := element("DAType", "id", typeID)
		for _, ch := range base.Children {
			v, err := b.attribute(ch, "BDA")
			if err != nil {
				return x, err
			}
			typ.Children = append(typ.Children, v)
		}
		b.das = append(b.das, typ)
	case "BOOLEAN":
		basic = "BOOLEAN"
	case "INTEGER":
		switch base.Size {
		case 8, 16, 24, 32, 64, 128:
			basic = fmt.Sprintf("INT%d", base.Size)
		}
	case "UNSIGNED":
		switch base.Size {
		case 8, 16, 24, 32:
			basic = fmt.Sprintf("INT%dU", base.Size)
		}
	case "FLOAT":
		if base.Size == 32 || base.Size == 64 {
			basic = fmt.Sprintf("FLOAT%d", base.Size)
		}
	case "UTC_TIME":
		basic = "Timestamp"
	case "BINARY_TIME":
		if base.Size == 6 {
			basic = "EntryTime"
		}
	case "VISIBLE_STRING":
		size := base.Size
		if size < 0 {
			size = -size
		}
		switch size {
		case 32, 64, 65, 129, 255:
			basic = fmt.Sprintf("VisString%d", size)
		}
		if size == 129 {
			switch t.Name {
			case "setSrcRef", "setTstRef", "setSrcCB", "setTstCB", "objRef", "datSet":
				basic = "ObjRef"
			}
		}
	case "MMS_STRING":
		basic = "Unicode255"
	case "OCTET_STRING":
		size := base.Size
		if size < 0 {
			size = -size
		}
		switch size {
		case 6, 16, 64:
			basic = fmt.Sprintf("Octet%d", size)
		case 8:
			basic = "EntryID"
		}
	case "BIT_STRING":
		size := base.Size
		if size < 0 {
			size = -size
		}
		switch t.Name {
		case "q":
			if size == 13 {
				basic = "Quality"
			}
		case "Check":
			if size == 2 {
				basic = "Check"
			}
		case "stVal":
			if size == 2 {
				basic = "Dbpos"
			}
		case "TrgOps", "trgOps":
			if size == 6 {
				basic = "TrgOps"
			}
		case "OptFlds", "optFlds":
			if size == 10 {
				basic = "OptFlds"
			}
		}
	}
	if basic == "" {
		return x, fmt.Errorf("unresolved SCL type %s (%s/%d)", t.Name, base.Kind, base.Size)
	}
	x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: "bType"}, Value: basic})
	if typeID != "" {
		x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: "type"}, Value: typeID})
	}
	return x, nil
}

func exportMember(ref string, ldMap map[string]string) (xmlElement, error) {
	x := element("FCDA")
	open := strings.LastIndex(ref, "[")
	if open < 0 || !strings.HasSuffix(ref, "]") {
		return x, fmt.Errorf("unsupported member %s", ref)
	}
	fc := ref[open+1 : len(ref)-1]
	path := ref[:open]
	domain, path, ok := strings.Cut(path, "/")
	if !ok || ldMap[domain] == "" {
		return x, fmt.Errorf("unknown DataSet domain: %s", ref)
	}
	ln, data, ok := strings.Cut(path, ".")
	if !ok {
		return x, fmt.Errorf("invalid DataSet member %s", ref)
	}
	prefix, class, inst, err := splitLN(ln)
	if err != nil {
		return x, err
	}
	if strings.ContainsAny(data, "()") {
		return x, fmt.Errorf("array DataSet member omitted: %s", ref)
	}
	parts := strings.Split(data, ".")
	doName := parts[0]
	daName := strings.Join(parts[1:], ".")
	x = element("FCDA", "ldInst", ldMap[domain], "lnClass", class, "lnInst", inst, "doName", doName, "fc", fc)
	if prefix != "" {
		x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: "prefix"}, Value: prefix})
	}
	if daName != "" {
		x.Attr = append(x.Attr, xml.Attr{Name: xml.Name{Local: "daName"}, Value: daName})
	}
	return x, nil
}
