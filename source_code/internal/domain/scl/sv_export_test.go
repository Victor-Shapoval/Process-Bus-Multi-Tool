package scl

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestMarshalSVPublisherBuildsCompatible92LEModel(t *testing.T) {
	vlanID := uint16(3)
	vlanPriority := uint8(4)
	data, err := MarshalSVPublisher(SVPublisher{
		Name:                  "PBMT SV",
		DstMAC:                "01:0c:cd:04:00:01",
		AppID:                 0x4001,
		SvID:                  "PBMTMU0101",
		DatSet:                "MU01/LLN0$PhsMeas1",
		ConfRev:               7,
		VLANID:                &vlanID,
		VLANPriority:          &vlanPriority,
		SmpRate:               80,
		SampleTimingFrequency: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, xml.Header) || !strings.Contains(text, `xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"`) {
		t.Fatalf("missing XML declaration or SCL namespace: %s", text[:min(len(text), 300)])
	}
	for _, want := range []string{
		`xsi:type="tP_MAC-Address"`,
		`xsi:type="tP_APPID"`,
		`xsi:type="tP_VLAN-ID"`,
		`xsi:type="tP_VLAN-PRIORITY"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated ICD does not contain %s", want)
		}
	}

	var doc sclDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("generated ICD is not XML: %v", err)
	}
	if doc.Version != "" || doc.Revision != "" {
		t.Fatalf("9-2LE ICD must use the Edition 1 shape: version=%q revision=%q", doc.Version, doc.Revision)
	}
	if len(doc.IEDs) != 1 || doc.IEDs[0].Name != "PBMT_SV" {
		t.Fatalf("unexpected IED identity: %+v", doc.IEDs)
	}
	connected := doc.Communication.SubNetworks[0].ConnectedAPs[0]
	if connected.IEDName != doc.IEDs[0].Name || len(connected.SMVs) != 1 {
		t.Fatalf("Communication/IED mismatch: connected=%+v IED=%q", connected, doc.IEDs[0].Name)
	}
	smv := connected.SMVs[0]
	if smv.LDInst != "MU01" || smv.CBName != "MSVCB01" {
		t.Fatalf("unexpected SMV communication reference: %+v", smv)
	}
	if len(smv.Address.Params) != 4 {
		t.Fatalf("unexpected SMV address parameter count: %+v", smv.Address.Params)
	}
	wantAddressValues := []string{"01-0C-CD-04-00-01", "4001", "003", "4"}
	for i, want := range wantAddressValues {
		if got := smv.Address.Params[i].Value; got != want {
			t.Fatalf("SMV address parameter #%d: want %q, got %q", i+1, want, got)
		}
	}

	ld := doc.IEDs[0].AccessPoints[0].Server.LDevices[0]
	if ld.Inst != "MU01" || ld.LDName != "" {
		t.Fatalf("unexpected logical device: %+v", ld)
	}
	if len(ld.LN0.DataSets) != 1 || ld.LN0.DataSets[0].Name != "PhsMeas1" {
		t.Fatalf("unexpected SV dataset: %+v", ld.LN0.DataSets)
	}
	fcdas := ld.LN0.DataSets[0].FCDAs
	if len(fcdas) != 16 {
		t.Fatalf("9-2LE dataset entries: want 16, got %d", len(fcdas))
	}
	wantRefs := []struct {
		lnClass string
		lnInst  string
		doName  string
		daName  string
	}{
		{"TCTR", "1", "Amp", "instMag.i"}, {"TCTR", "1", "Amp", "q"},
		{"TCTR", "2", "Amp", "instMag.i"}, {"TCTR", "2", "Amp", "q"},
		{"TCTR", "3", "Amp", "instMag.i"}, {"TCTR", "3", "Amp", "q"},
		{"TCTR", "4", "Amp", "instMag.i"}, {"TCTR", "4", "Amp", "q"},
		{"TVTR", "1", "Vol", "instMag.i"}, {"TVTR", "1", "Vol", "q"},
		{"TVTR", "2", "Vol", "instMag.i"}, {"TVTR", "2", "Vol", "q"},
		{"TVTR", "3", "Vol", "instMag.i"}, {"TVTR", "3", "Vol", "q"},
		{"TVTR", "4", "Vol", "instMag.i"}, {"TVTR", "4", "Vol", "q"},
	}
	for i, want := range wantRefs {
		got := fcdas[i]
		if got.LDInst != "MU01" || got.LNClass != want.lnClass || got.LNInst != want.lnInst || got.DOName != want.doName || got.DAName != want.daName || got.FC != "MX" {
			t.Fatalf("FCDA #%d: got %+v, want %+v", i+1, got, want)
		}
	}

	if len(ld.LN0.SampledValueControls) != 1 {
		t.Fatalf("SampledValueControl missing: %+v", ld.LN0)
	}
	control := ld.LN0.SampledValueControls[0]
	if control.Name != "MSVCB01" || control.SmvID != "PBMTMU0101" || !control.Multicast || control.DatSet != "PhsMeas1" || control.SmpRate != 80 || control.NofASDU != 1 || control.ConfRev != 7 {
		t.Fatalf("unexpected SampledValueControl: %+v", control)
	}
	if control.SmvOpts.RefreshTime || control.SmvOpts.SampleRate || control.SmvOpts.DataSet || control.SmvOpts.Security {
		t.Fatalf("unexpected 9-2LE SmvOpts: %+v", control.SmvOpts)
	}
	if len(ld.LNs) != 9 {
		t.Fatalf("logical nodes: want LPHD + 4 TCTR + 4 TVTR, got %d", len(ld.LNs))
	}
	assertSVTypeReferencesResolve(t, doc.DataTypes)
	assertSVScale(t, ld.LNs, "TCTR", "0.001")
	assertSVScale(t, ld.LNs, "TVTR", "0.01")
}

func assertSVTypeReferencesResolve(t *testing.T, templates dataTypeTemplates) {
	t.Helper()
	lnTypes := make(map[string]bool)
	doTypes := make(map[string]bool)
	daTypes := make(map[string]bool)
	enumTypes := make(map[string]bool)
	for _, typ := range templates.LNodeTypes {
		lnTypes[typ.ID] = true
	}
	for _, typ := range templates.DOTypes {
		doTypes[typ.ID] = true
	}
	for _, typ := range templates.DATypes {
		daTypes[typ.ID] = true
	}
	for _, typ := range templates.EnumTypes {
		enumTypes[typ.ID] = true
	}
	if len(lnTypes) != 4 {
		t.Fatalf("logical-node type count: want 4, got %d", len(lnTypes))
	}
	for _, typ := range templates.LNodeTypes {
		for _, object := range typ.DOs {
			if !doTypes[object.Type] {
				t.Errorf("LNodeType %q references missing DOType %q", typ.ID, object.Type)
			}
		}
	}
	for _, typ := range templates.DOTypes {
		for _, attribute := range typ.DAs {
			switch attribute.BType {
			case "Struct":
				if !daTypes[attribute.Type] {
					t.Errorf("DOType %q references missing DAType %q", typ.ID, attribute.Type)
				}
			case "Enum":
				if !enumTypes[attribute.Type] {
					t.Errorf("DOType %q references missing EnumType %q", typ.ID, attribute.Type)
				}
			}
		}
	}
}

func assertSVScale(t *testing.T, nodes []logicalNode, class, want string) {
	t.Helper()
	for _, node := range nodes {
		if node.LNClass != class || node.Inst != "1" {
			continue
		}
		for _, object := range node.DOIs {
			for _, sub := range object.SDIs {
				if sub.Name != "sVC" {
					continue
				}
				for _, attribute := range sub.DAIs {
					if attribute.Name == "scaleFactor" && len(attribute.Values) == 1 && attribute.Values[0] == want {
						return
					}
				}
			}
		}
		t.Fatalf("%s scale factor %s not found", class, want)
	}
	t.Fatalf("%s1 logical node not found", class)
}

func TestMarshalSVPublisherUsesDefaultDatasetName(t *testing.T) {
	data, err := MarshalSVPublisher(SVPublisher{
		Name:                  "PBMT",
		DstMAC:                "01:0c:cd:04:00:01",
		AppID:                 0x4001,
		SvID:                  "PBMTMU0101",
		SmpRate:               80,
		SampleTimingFrequency: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `DataSet name="PhsMeas1"`) {
		t.Fatal("default PhsMeas1 dataset is missing")
	}
}

func TestMarshalSVPublisherRejectsInvalidConfiguration(t *testing.T) {
	_, err := MarshalSVPublisher(SVPublisher{Name: "PBMT", DstMAC: "bad", SvID: "PBMTMU0101", SmpRate: 80, SampleTimingFrequency: 50})
	if err == nil || !strings.Contains(err.Error(), "destination MAC") {
		t.Fatalf("expected destination MAC error, got %v", err)
	}
	_, err = MarshalSVPublisher(SVPublisher{Name: "PBMT", DstMAC: "01:0c:cd:04:00:01", SvID: "PBMTMU0101", DatSet: "bad name", SmpRate: 80, SampleTimingFrequency: 50})
	if err == nil || !strings.Contains(err.Error(), "dataset name") {
		t.Fatalf("expected dataset name error, got %v", err)
	}
	_, err = MarshalSVPublisher(SVPublisher{Name: "PBMT", DstMAC: "01:0c:cd:04:00:01", SvID: "PBMTMU0101", SmpRate: 256, SampleTimingFrequency: 50})
	if err == nil || !strings.Contains(err.Error(), "supports smp_rate 80") {
		t.Fatalf("expected unsupported 256-sample Publisher error, got %v", err)
	}
}
