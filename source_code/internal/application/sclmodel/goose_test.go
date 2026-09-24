package sclmodel

import (
	"net"
	"os"
	"strings"
	"testing"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

func gooseSCLFixture(t *testing.T) []byte {
	t.Helper()
	m := testModel()
	m.Devices[0].Nodes[0].GOOSE = []GooseControl{{Name: "events", DataSet: "IEDLD0/LLN0$ds", GoID: "go-events", MAC: "01-0C-CD-01-00-22", APPID: 3, VLAN: 2, Priority: 4, ConfRev: 7}}
	raw, warnings, err := ExportICD(m)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("fixture export: %v %v", err, warnings)
	}
	return raw
}

func TestGooseSCLMetadataAndOrderedMembers(t *testing.T) {
	streams, err := ParseGooseSCL(gooseSCLFixture(t))
	if err != nil || len(streams) != 1 {
		t.Fatalf("parse: %v %+v", err, streams)
	}
	g := streams[0]
	if g.FrameError != "" || g.SchemaError != "" {
		t.Fatalf("stream errors: %+v", g)
	}
	if g.AppID != 3 || g.GoID != "go-events" || g.GocbRef != "IEDLD0/LLN0$GO$events" || g.DatSet != "IEDLD0/LLN0$ds" || g.ConfRev != 7 || *g.VLANID != 2 || *g.VLANPriority != 4 {
		t.Fatalf("metadata: %+v", g)
	}
	if len(g.Entries) != 2 || g.Entries[0].Name != "IEDLD0/LLN0.Loc.stVal" || g.Entries[1].Name != "IEDLD0/LLN0.Loc.q" || g.Entries[1].Size != 13 {
		t.Fatalf("ordered schema: %+v", g.Entries)
	}
	mac, _ := net.ParseMAC(g.DstMAC)
	p := &goose.PDU{DstMAC: mac, AppID: g.AppID, GocbRef: g.GocbRef, DatSet: g.DatSet, GoID: g.GoID, ConfRev: g.ConfRev, VLAN: &ethernet.VLANTag{VID: 2, Priority: 4}, NumDatSetEntries: 2, AllData: []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}, {Type: goose.DataTypeBitString, BitLength: 13, Bytes: []byte{0, 0}}}}
	if err := g.ValidatePDU(p); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*goose.PDU){
		"revision": func(p *goose.PDU) { p.ConfRev++ }, "count": func(p *goose.PDU) { p.NumDatSetEntries++ },
		"type": func(p *goose.PDU) { p.AllData[0].Type = goose.DataTypeInteger }, "quality": func(p *goose.PDU) { p.AllData[1].BitLength = 16 },
		"reference": func(p *goose.PDU) { p.GocbRef += "other" }, "dataset": func(p *goose.PDU) { p.DatSet += "other" },
		"appID": func(p *goose.PDU) { p.AppID++ }, "MAC": func(p *goose.PDU) { p.DstMAC = nil }, "VLAN": func(p *goose.PDU) { p.VLAN = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *p
			copy.AllData = append([]goose.DataValue{}, p.AllData...)
			change(&copy)
			if g.ValidatePDU(&copy) == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
}

func TestGooseSCLUnrelatedReportsAndMissingAddress(t *testing.T) {
	raw := string(gooseSCLFixture(t))
	raw = strings.Replace(raw, "<GSEControl ", `<ReportControl name="irrelevant" indexed="true"/><GSEControl `, 1)
	streams, err := ParseGooseSCL([]byte(raw))
	if err != nil || len(streams) != 1 || streams[0].SchemaError != "" {
		t.Fatalf("unrelated report blocked import: %v %+v", err, streams)
	}
	raw = strings.Replace(raw, `type="APPID"`, `type="missing"`, 1)
	streams, err = ParseGooseSCL([]byte(raw))
	if err != nil || streams[0].FrameError == "" {
		t.Fatal("missing APPID silently treated as zero")
	}
}

func TestGooseSCLWholeObjectAndArrays(t *testing.T) {
	raw := string(gooseSCLFixture(t))
	raw = strings.Replace(raw, ` daName="stVal"`, "", 1)
	raw = strings.Replace(raw, `<DA name="stVal"`, `<DA count="2" name="stVal"`, 1)
	streams, err := ParseGooseSCL([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	g := streams[0]
	if g.SchemaError != "" || g.Entries[0].Kind != "STRUCTURE" || g.Entries[0].Children[0].Kind != "ARRAY" {
		t.Fatalf("whole-object shape lost: %+v", g)
	}
	value := goose.DataValue{Type: goose.DataTypeStructure, Children: []goose.DataValue{{Type: goose.DataTypeArray, Children: []goose.DataValue{{Type: goose.DataTypeBoolean}, {Type: goose.DataTypeBoolean}}}, {Type: goose.DataTypeBitString, BitLength: 13}, {Type: goose.DataTypeUTCTime}}}
	if !matchesGooseType(g.Entries[0], value) {
		t.Fatal("nested array rejected")
	}
	value.Children[0].Children[1].Type = goose.DataTypeInteger
	if matchesGooseType(g.Entries[0], value) {
		t.Fatal("nested mismatch accepted")
	}
	raw = strings.Replace(raw, `count="2"`, `count="200000"`, 1)
	streams, err = ParseGooseSCL([]byte(raw))
	if err != nil || streams[0].SchemaError == "" {
		t.Fatal("oversized expanded dataset accepted")
	}
}

func TestGooseSCLCapturedICD(t *testing.T) {
	path := os.Getenv("PBMT_GOOSE_ICD_FIXTURE")
	if path == "" {
		t.Skip("set PBMT_GOOSE_ICD_FIXTURE to test an external ICD")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := ParseGooseSCL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 {
		t.Fatal("no streams")
	}
	for _, g := range streams {
		if g.FrameError != "" || g.SchemaError != "" {
			t.Errorf("%s: %s %s", g.GocbRef, g.FrameError, g.SchemaError)
		}
		t.Logf("%s: %d entries", g.GocbRef, len(g.Entries))
	}
}
