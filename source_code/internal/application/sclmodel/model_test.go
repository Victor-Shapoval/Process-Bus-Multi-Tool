package sclmodel

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// A locally captured model can be checked without committing device data.
func TestExportCapturedModel(t *testing.T) {
	path := os.Getenv("PBMT_MODEL_FIXTURE")
	if path == "" {
		t.Skip("set PBMT_MODEL_FIXTURE to a captured scl_model.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var model Model
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	data, warnings, err := ExportICD(&model)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, warning := range warnings {
		if strings.Contains(warning, "ambiguous FC") || strings.Contains(warning, "CDC is not recoverable") || strings.Contains(warning, "unresolved SCL type") {
			t.Errorf("unresolved FC partition: %s", warning)
		}
		category := warning
		if i := strings.Index(warning, ": "); i >= 0 {
			category = warning[i+2:]
		}
		counts[category]++
	}
	t.Logf("standard DOs=%d; SDOs=%d; warnings=%d; categories=%v", strings.Count(string(data), "<DO "), strings.Count(string(data), "<SDO "), len(warnings), counts)
}

func TestAdditionalCDCsAndWireTypes(t *testing.T) {
	fields := func(names ...string) []*Type {
		var result []*Type
		for _, name := range names {
			v := &Type{Name: name, Kind: "INTEGER", Size: 8}
			switch name {
			case "objRef", "setSrcRef", "rptID", "datSet":
				v.Kind = "VISIBLE_STRING"
				v.Size = -129
			case "setTm", "t":
				v.Kind = "UTC_TIME"
			case "ctlVal", "rptEna", "purgeBuf", "resv":
				v.Kind = "BOOLEAN"
			case "origin":
				v.Kind = "STRUCTURE"
				v.Children = []*Type{{Name: "orCat", Kind: "INTEGER", Size: 8}, {Name: "orIdent", Kind: "OCTET_STRING", Size: 64}}
			case "entryID":
				v.Kind = "OCTET_STRING"
				v.Size = 8
			case "optFlds":
				v.Kind = "BIT_STRING"
				v.Size = -10
			case "trgOps":
				v.Kind = "BIT_STRING"
				v.Size = -6
			}
			result = append(result, v)
		}
		return result
	}
	common := []string{"objRef", "serviceType", "errorCode", "t"}
	for _, tc := range []struct {
		cdc, fc string
		attrs   []*Type
	}{
		{"ORG", "SP", fields("setSrcRef")}, {"TSG", "SP", fields("setTm")},
		{"VSS", "ST", []*Type{{Name: "stVal", Kind: "VISIBLE_STRING", Size: -255}, {Name: "q", Kind: "BIT_STRING", Size: -13}, {Name: "t", Kind: "UTC_TIME"}}},
		{"CTS", "SR", fields(append(append([]string{}, common...), "ctlVal", "origin")...)},
		{"BTS", "SR", fields(append(append([]string{}, common...), "rptID", "purgeBuf", "entryID", "optFlds", "trgOps")...)},
		{"UTS", "SR", fields(append(append([]string{}, common...), "rptID", "resv", "optFlds", "trgOps")...)},
		{"STS", "SR", fields(append(append([]string{}, common...), "numOfSG", "actSG")...)},
	} {
		t.Run(tc.cdc, func(t *testing.T) {
			b := &exportBuilder{}
			id, err := b.object([]fcType{{fc: tc.fc, t: &Type{Name: "Object", Kind: "STRUCTURE", Children: tc.attrs}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, typ := range b.dos {
				if attribute(typ, "id") != id {
					continue
				}
				if attribute(typ, "cdc") != tc.cdc {
					t.Fatalf("CDC=%s", attribute(typ, "cdc"))
				}
				for _, da := range typ.Children {
					switch attribute(da, "name") {
					case "setSrcRef", "objRef":
						if attribute(da, "bType") != "ObjRef" {
							t.Fatal("object reference lost its SCL type")
						}
					case "optFlds":
						if attribute(da, "bType") != "OptFlds" {
							t.Fatal("report options not mapped")
						}
					case "trgOps":
						if attribute(da, "bType") != "TrgOps" {
							t.Fatal("trigger options not mapped")
						}
					}
				}
			}
		})
	}
}

func TestReportAndSettingControlExport(t *testing.T) {
	m := testModel()
	ln := &m.Devices[0].Nodes[0]
	ln.Settings = &SettingControl{NumGroups: 4, ActiveGroup: 2}
	ln.Reports = []ReportControl{{Name: "report01", ReportID: "event-report", DataSet: "IEDLD0/LLN0$ds", ConfRev: 7, BufferTime: 25, IntegrityPeriod: 2000, Triggers: ReportTriggers{DataChange: true, GI: true}, Options: ReportOptions{DataSet: true, SeqNum: true}}}
	ln.GOOSE = []GooseControl{{Name: "goose", DataSet: "IEDLD0/LLN0$ds", GoID: "events", MAC: "01-0C-CD-01-00-01"}}
	data, warnings, err := ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings: %v", warnings)
	}
	text := string(data)
	for _, expected := range []string{`<ReportControl name="report01"`, `indexed="false"`, `rptID="event-report"`, `confRev="7"`, `bufTime="25"`, `intgPd="2000"`, `<SettingControl numOfSGs="4" actSG="2"`} {
		if !strings.Contains(text, expected) {
			t.Errorf("missing %s", expected)
		}
	}
	if !(strings.Index(text, "<DataSet ") < strings.Index(text, "<ReportControl ") && strings.Index(text, "<ReportControl ") < strings.Index(text, "<GSEControl ") && strings.Index(text, "<GSEControl ") < strings.Index(text, "<SettingControl ")) {
		t.Fatal("invalid SCL child order")
	}
	ln.Reports[0].DataSet = "IEDLD0/LLN0$missing"
	data, warnings, err = ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "<ReportControl ") || len(warnings) != 2 {
		t.Fatal("dangling report DataSet reference exported")
	}
	ln.Reports[0].DataSet = ""
	data, warnings, err = ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<ReportControl ") || len(warnings) != 1 {
		t.Fatal("unassigned report DataSet rejected")
	}
}

func TestCachedControlSchemasDoNotInventConfigurationValues(t *testing.T) {
	m := testModel()
	ln := &m.Devices[0].Nodes[0]
	ln.Groups = append(ln.Groups, &Type{Name: "RP", Kind: "STRUCTURE", Children: []*Type{{Name: "report", Kind: "STRUCTURE"}}}, &Type{Name: "SP", Kind: "STRUCTURE", Children: []*Type{{Name: "SGCB", Kind: "STRUCTURE"}}})
	data, warnings, err := ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "<ReportControl ") || strings.Contains(string(data), "<SettingControl ") {
		t.Fatal("configuration invented from a type schema")
	}
	if len(warnings) != 3 || !strings.Contains(strings.Join(warnings, " "), "run Read Model again") {
		t.Fatalf("missing reread instruction: %v", warnings)
	}
}

func TestSubObjectsMergeFunctionalConstraintsInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		cdc   string
		names []string
	}{
		{"WYE", []string{"phsA", "phsB", "phsC"}}, {"SEQ", []string{"c1", "c2", "c3"}},
	} {
		for _, reversed := range []bool{false, true} {
			t.Run(tc.cdc+map[bool]string{false: "/CF-first", true: "/MX-first"}[reversed], func(t *testing.T) {
				var groups []fcType
				for _, fc := range []string{"CF", "DC", "MX"} {
					obj := &Type{Name: "A", Kind: "STRUCTURE"}
					for _, name := range tc.names {
						part := &Type{Name: name, Kind: "STRUCTURE"}
						switch fc {
						case "CF":
							part.Children = []*Type{{Name: "units", Kind: "STRUCTURE", Children: []*Type{{Name: "SIUnit", Kind: "INTEGER", Size: 8}}}}
						case "DC":
							part.Children = []*Type{{Name: "d", Kind: "VISIBLE_STRING", Size: -255}}
						case "MX":
							part.Children = []*Type{{Name: "cVal", Kind: "STRUCTURE", Children: []*Type{{Name: "mag", Kind: "STRUCTURE", Children: []*Type{{Name: "f", Kind: "FLOAT", Size: 32}}}}}, {Name: "q", Kind: "BIT_STRING", Size: -13}, {Name: "t", Kind: "UTC_TIME"}}
						}
						obj.Children = append(obj.Children, part)
					}
					groups = append(groups, fcType{fc: fc, t: obj})
				}
				if reversed {
					groups[0], groups[2] = groups[2], groups[0]
				}
				b := &exportBuilder{}
				id, err := b.object(groups)
				if err != nil {
					t.Fatal(err)
				}
				types := map[string]xmlElement{}
				for _, typ := range b.dos {
					types[attribute(typ, "id")] = typ
				}
				root := types[id]
				if attribute(root, "cdc") != tc.cdc || len(root.Children) != 3 {
					t.Fatalf("root=%+v", root)
				}
				for _, sdo := range root.Children {
					if sdo.Name != "SDO" {
						t.Fatalf("nested object exported as %s", sdo.Name)
					}
					child := types[attribute(sdo, "type")]
					if attribute(child, "cdc") != "CMV" {
						t.Fatal("lost CMV type")
					}
					got := map[string]string{}
					for _, da := range child.Children {
						got[attribute(da, "name")] = attribute(da, "fc")
					}
					for name, fc := range map[string]string{"units": "CF", "d": "DC", "cVal": "MX", "q": "MX", "t": "MX"} {
						if got[name] != fc {
							t.Errorf("%s: FC=%q, want %q", name, got[name], fc)
						}
					}
				}
			})
		}
	}
}

func TestLeafFunctionalConstraintConflictsRemainErrors(t *testing.T) {
	makePart := func(fc string, size int) fcType {
		return fcType{fc: fc, t: &Type{Name: "Setting", Kind: "STRUCTURE", Children: []*Type{{Name: "setVal", Kind: "INTEGER", Size: size}}}}
	}
	for _, groups := range [][]fcType{{makePart("MX", 32), makePart("CF", 32)}, {makePart("SG", 32), makePart("SE", 16)}} {
		if _, err := (&exportBuilder{}).object(groups); err == nil {
			t.Fatal("conflicting attribute definitions silently merged")
		}
	}
	for _, groups := range [][]fcType{{makePart("SG", 32), makePart("SE", 32)}, {makePart("SE", 32), makePart("SG", 32)}} {
		b := &exportBuilder{}
		_, err := b.object(groups)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.dos[0].Children) != 1 || attribute(b.dos[0].Children[0], "fc") != "SG" {
			t.Fatal("SG/SE was not represented as one setting")
		}
	}
}

func TestMeasuredValueWithInstantaneousMagnitudeIsMV(t *testing.T) {
	attrs := map[string][]fcType{"mag": {{t: &Type{Kind: "STRUCTURE"}}}, "instMag": {{t: &Type{Kind: "STRUCTURE"}}}}
	if got := inferCDC(attrs); got != "MV" {
		t.Fatalf("CDC=%s, want MV", got)
	}
	delete(attrs, "mag")
	if got := inferCDC(attrs); got != "SAV" {
		t.Fatalf("CDC=%s, want SAV", got)
	}
}

func testModel() *Model {
	object := &Type{Name: "Loc", Kind: "STRUCTURE", Children: []*Type{
		{Name: "stVal", Kind: "BOOLEAN"}, {Name: "q", Kind: "BIT_STRING", Size: 13}, {Name: "t", Kind: "UTC_TIME"},
	}}
	node := LogicalNode{Name: "LLN0", Groups: []*Type{{Name: "ST", Kind: "STRUCTURE", Children: []*Type{object}}},
		DataSets: []DataSet{{Name: "ds", Members: []string{"IEDLD0/LLN0.Loc.stVal[ST]", "IEDLD0/LLN0.Loc.q[ST]"}}},
	}
	return &Model{Endpoint: Endpoint{IP: "127.0.0.1", Port: 102}, Devices: []LogicalDevice{{Name: "IEDLD0", Nodes: []LogicalNode{node}}}}
}

func TestTreePreservesArrayAccessAndFunctionalConstraint(t *testing.T) {
	m := testModel()
	m.Devices[0].Nodes[0].Groups[0].Children[0].Children = append(m.Devices[0].Nodes[0].Groups[0].Children[0].Children, &Type{Name: "samples", Kind: "ARRAY", Size: 2, Children: []*Type{{Kind: "STRUCTURE", Children: []*Type{{Name: "f", Kind: "FLOAT", Size: 32}}}}})
	tree, err := BuildTree(m)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, n := range tree.Nodes {
		if n.Target != nil && strings.Contains(n.Target.Item, "samples") {
			count++
			if n.Target.Item != "LLN0$ST$Loc$samples" || len(n.Target.Path) != 2 || n.Target.Path[1] != 0 {
				t.Fatalf("array target: %+v", n.Target)
			}
		}
	}
	if count != 2 {
		t.Fatalf("array element count: %d", count)
	}
	for id, n := range tree.Nodes {
		if n.Target != nil {
			if tree.Expanded(id, func(string) bool { return false }) {
				t.Fatal("collapsed target treated as visible")
			}
			if !tree.Expanded(id, func(string) bool { return true }) {
				t.Fatal("open target treated as hidden")
			}
		}
	}
}

func TestICDContainsTypesOrderedMembersAndNoLiveValues(t *testing.T) {
	bytes, warnings, err := ExportICD(testModel())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Fatal("missing reconstruction notice")
	}
	var doc struct {
		XMLName xml.Name
		IED     struct {
			AccessPoint struct {
				Server struct {
					Devices []struct {
						LN0 struct {
							DataSet struct {
								Members []struct {
									DA string `xml:"daName,attr"`
								} `xml:"FCDA"`
							} `xml:"DataSet"`
						} `xml:"LN0"`
					} `xml:"LDevice"`
				} `xml:"Server"`
			} `xml:"AccessPoint"`
		} `xml:"IED"`
	}
	if err := xml.Unmarshal(bytes, &doc); err != nil {
		t.Fatal(err)
	}
	members := doc.IED.AccessPoint.Server.Devices[0].LN0.DataSet.Members
	if len(members) != 2 || members[0].DA != "stVal" || members[1].DA != "q" {
		t.Fatalf("member order: %+v", members)
	}
	text := string(bytes)
	for _, want := range []string{`cdc="SPS"`, `bType="Quality"`, `bType="Timestamp"`, `ldName="IEDLD0"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(text, "<Val>") {
		t.Fatal("live values exported as initial settings")
	}
}

func TestICDOmitsUnresolvedTypesWithoutPrivateMetadata(t *testing.T) {
	m := testModel()
	m.Devices[0].Nodes[0].Groups[0].Children = append(m.Devices[0].Nodes[0].Groups[0].Children, &Type{Name: "VendorObject", Kind: "STRUCTURE", Children: []*Type{{Name: "x", Kind: "BIT_STRING", Size: 17}}})
	m.Devices[0].Nodes[0].DataSets = append(m.Devices[0].Nodes[0].DataSets, DataSet{Name: "unresolved", Members: []string{"IEDLD0/LLN0.Loc.stVal[ST]", "IEDLD0/LLN0.VendorObject.x[ST]"}})
	bytes, warnings, err := ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "VendorObject") {
		t.Fatal("silent omission of unknown CDC")
	}
	if strings.Contains(string(bytes), "VendorObject") || strings.Contains(string(bytes), "<Private") {
		t.Fatal("unrepresentable object or raw model was exported")
	}
	if strings.Contains(string(bytes), `<DataSet name="unresolved"`) {
		t.Fatal("export contains dangling DataSet references")
	}
}

func TestICDResolvesSubDataObjectMembers(t *testing.T) {
	m := testModel()
	phase := func(name string) *Type {
		return &Type{Name: name, Kind: "STRUCTURE", Children: []*Type{{Name: "mag", Kind: "STRUCTURE", Children: []*Type{{Name: "f", Kind: "FLOAT", Size: 32}}}, {Name: "q", Kind: "BIT_STRING", Size: -13}, {Name: "t", Kind: "UTC_TIME"}}}
	}
	m.Devices[0].Nodes[0].Groups = append(m.Devices[0].Nodes[0].Groups, &Type{Name: "MX", Kind: "STRUCTURE", Children: []*Type{{Name: "A", Kind: "STRUCTURE", Children: []*Type{phase("phsA"), phase("phsB"), phase("phsC")}}}})
	m.Devices[0].Nodes[0].DataSets = append(m.Devices[0].Nodes[0].DataSets, DataSet{Name: "phasors", Members: []string{"IEDLD0/LLN0.A.phsA.mag.f[MX]"}})
	bytes, warnings, err := ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bytes), `doName="A.phsA"`) || !strings.Contains(string(bytes), `daName="mag.f"`) {
		t.Fatalf("nested reference not resolved; warnings=%v", warnings)
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, e := range []Endpoint{{"", 102}, {"host", 102}, {"127.0.0.1", 0}, {"127.0.0.1", 65536}} {
		if e.Validate() == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	if err := (Endpoint{"::1", 102}).Validate(); err != nil {
		t.Fatal(err)
	}
}
