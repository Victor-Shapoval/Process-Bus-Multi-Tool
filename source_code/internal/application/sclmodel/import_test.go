package sclmodel

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func targetAddresses(t *testing.T, m *Model) []string {
	t.Helper()
	tree, err := BuildTree(m)
	if err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, node := range tree.Nodes {
		if v := node.Target; v != nil {
			addresses = append(addresses, fmt.Sprintf("%s/%s:%v", v.Domain, v.Item, v.Path))
		}
	}
	sort.Strings(addresses)
	return addresses
}

func TestICDRoundTripPreservesDataTargetsAndControls(t *testing.T) {
	m := testModel()
	m.Endpoint = Endpoint{IP: "10.10.5.22", Port: 102}
	ln := &m.Devices[0].Nodes[0]
	phase := func(name string, measured bool) *Type {
		attrs := []*Type{{Name: "units", Kind: "STRUCTURE", Children: []*Type{{Name: "SIUnit", Kind: "INTEGER", Size: 8}}}}
		if measured {
			attrs = []*Type{{Name: "cVal", Kind: "STRUCTURE", Children: []*Type{{Name: "mag", Kind: "STRUCTURE", Children: []*Type{{Name: "f", Kind: "FLOAT", Size: 32}}}}}, {Name: "q", Kind: "BIT_STRING", Size: 13}, {Name: "t", Kind: "UTC_TIME"}}
		}
		return &Type{Name: name, Kind: "STRUCTURE", Children: attrs}
	}
	for _, fc := range []string{"CF", "MX"} {
		ln.Groups = append(ln.Groups, &Type{Name: fc, Kind: "STRUCTURE", Children: []*Type{{Name: "A", Kind: "STRUCTURE", Children: []*Type{phase("phsA", fc == "MX"), phase("phsB", fc == "MX"), phase("phsC", fc == "MX")}}}})
	}
	ln.Groups[0].Children[0].Children = append(ln.Groups[0].Children[0].Children, &Type{Name: "samples", Kind: "ARRAY", Size: 3, Children: []*Type{{Kind: "STRUCTURE", Children: []*Type{{Name: "f", Kind: "FLOAT", Size: 32}, {Name: "valid", Kind: "BOOLEAN"}}}}})
	ln.DataSets = append(ln.DataSets, DataSet{Name: "nested", Members: []string{"IEDLD0/LLN0.A.phsA.cVal.mag.f[MX]", "IEDLD0/LLN0.Loc.q[ST]"}})
	ln.Reports = []ReportControl{{Name: "events01", ReportID: "events", DataSet: "IEDLD0/LLN0.ds", ConfRev: 6, BufferTime: 25, IntegrityPeriod: 1000, Buffered: true, Triggers: ReportTriggers{true, true, true, true, true}, Options: ReportOptions{true, true, true, true, true, true, true, true}}}
	ln.Settings = &SettingControl{NumGroups: 4, ActiveGroup: 2}
	ln.GOOSE = []GooseControl{{Name: "gcb", DataSet: "IEDLD0/LLN0.ds", GoID: "go-events", MAC: "01-0C-CD-01-00-01", ConfRev: 6, APPID: 0x1001, VLAN: 123, Priority: 4, MinTime: 4, MaxTime: 1000}}
	raw, warnings, err := ExportICD(m)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("export: %v %v", err, warnings)
	}
	if strings.Contains(string(raw), "Private") {
		t.Fatal("Private metadata saved")
	}
	restored, err := ImportICD(raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Endpoint != m.Endpoint || !restored.ReadAt.IsZero() {
		t.Fatal("endpoint lost or discovery time fabricated")
	}
	if got, want := targetAddresses(t, restored), targetAddresses(t, m); !reflect.DeepEqual(got, want) {
		t.Fatalf("MMS targets changed:\ngot %v\nwant %v", got, want)
	}
	back := restored.Devices[0].Nodes[0]
	if !reflect.DeepEqual(back.DataSets, ln.DataSets) || !reflect.DeepEqual(back.Reports, ln.Reports) || !reflect.DeepEqual(back.GOOSE, ln.GOOSE) || !reflect.DeepEqual(back.Settings, ln.Settings) {
		t.Fatalf("configuration changed: %+v", back)
	}
	second, warnings, err := ExportICD(restored)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("re-export: %v %v", err, warnings)
	}
	if _, err := ImportICD(second); err != nil {
		t.Fatal(err)
	}
}

func TestImportICDRejectsInvalidOrAmbiguousModels(t *testing.T) {
	raw, _, err := ExportICD(testModel())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for name, data := range map[string]string{
		"empty": "", "broken": "<SCL>", "multiple roots": text + text,
		"multiple IEDs":     strings.Replace(text, "</SCL>", `<IED name="other"/></SCL>`, 1),
		"missing reference": strings.Replace(text, `lnType="PBMT_T1"`, `lnType="missing"`, 1),
		"recursive DO":      strings.Replace(text, `<DA name="stVal"`, `<SDO name="cycle" type="PBMT_T2"/><DA name="stVal"`, 1),
		"unsupported type":  strings.Replace(text, `bType="BOOLEAN"`, `bType="unknown"`, 1),
		"invalid width":     strings.Replace(text, `bType="BOOLEAN"`, `bType="FLOAT128"`, 1),
		"oversized array":   strings.Replace(text, `<DA name="stVal"`, `<DA count="999999999" name="stVal"`, 1),
		"bad IP":            strings.Replace(text, `type="IP">`, `type="IP">invalid`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ImportICD([]byte(data)); err == nil {
				t.Fatal("invalid ICD accepted")
			}
		})
	}
}

func TestICDOmissionDoesNotLeavePartialTypes(t *testing.T) {
	m := testModel()
	obj := &Type{Name: "Broken", Kind: "STRUCTURE", Children: []*Type{{Name: "stVal", Kind: "BOOLEAN"}, {Name: "details", Kind: "STRUCTURE", Children: []*Type{{Name: "nested", Kind: "STRUCTURE", Children: []*Type{{Name: "v", Kind: "BOOLEAN"}}}, {Name: "bad", Kind: "BIT_STRING", Size: 17}}}}}
	m.Devices[0].Nodes[0].Groups[0].Children = append(m.Devices[0].Nodes[0].Groups[0].Children, obj)
	raw, warnings, err := ExportICD(m)
	if err != nil || len(warnings) < 2 {
		t.Fatalf("export: %v %v", err, warnings)
	}
	for _, unwanted := range []string{"Private", "Broken", "nested", "details"} {
		if strings.Contains(string(raw), unwanted) {
			t.Fatalf("discarded type retained: %s", unwanted)
		}
	}
	if _, err := ImportICD(raw); err != nil {
		t.Fatal(err)
	}
}
