package scl

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestMarshalGoosePublisherBuildsConsistentModel(t *testing.T) {
	vlanID := uint16(100)
	vlanPriority := uint8(4)
	data, err := MarshalGoosePublisher(GoosePublisher{
		Name:         "PBMT GOOSE",
		DstMAC:       "01:0c:cd:01:00:01",
		AppID:        1,
		GocbRef:      "IED1/LLN0$GO$Control",
		DatSet:       "IED1/LLN0$DataSet1",
		GoID:         "CTRL1",
		ConfRev:      7,
		VLANID:       &vlanID,
		VLANPriority: &vlanPriority,
		MinTimeMS:    10,
		MaxTimeMS:    2000,
		Dataset: []DatasetEntry{
			{Name: "Signal1.stVal", Type: "bool"},
			{Name: "Signal1.q", Type: "quality"},
			{Name: "Signal2.mag.f", Type: "float"},
			{Name: "Signal2.q", Type: "quality"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), xml.Header) {
		t.Fatal("missing XML header")
	}

	var doc struct {
		Version       string `xml:"version,attr"`
		Revision      string `xml:"revision,attr"`
		Communication struct {
			SubNetwork struct {
				ConnectedAP struct {
					IEDName string `xml:"iedName,attr"`
					GSE     struct {
						LDInst  string `xml:"ldInst,attr"`
						CBName  string `xml:"cbName,attr"`
						Address struct {
							Params []addressParam `xml:"P"`
						} `xml:"Address"`
					} `xml:"GSE"`
				} `xml:"ConnectedAP"`
			} `xml:"SubNetwork"`
		} `xml:"Communication"`
		IED struct {
			Name        string `xml:"name,attr"`
			AccessPoint struct {
				Server struct {
					LDevice struct {
						Inst   string `xml:"inst,attr"`
						LDName string `xml:"ldName,attr"`
						LN0    struct {
							DataSet struct {
								Name  string `xml:"name,attr"`
								FCDAs []fcda `xml:"FCDA"`
							} `xml:"DataSet"`
							GSEControl gseControl `xml:"GSEControl"`
						} `xml:"LN0"`
					} `xml:"LDevice"`
				} `xml:"Server"`
			} `xml:"AccessPoint"`
		} `xml:"IED"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("generated document is not XML: %v", err)
	}

	if doc.Version != "2007" || doc.Revision != "B" {
		t.Fatalf("unexpected SCL edition: version=%q revision=%q", doc.Version, doc.Revision)
	}
	if doc.IED.Name != "PBMT_GOOSE" || doc.Communication.SubNetwork.ConnectedAP.IEDName != doc.IED.Name {
		t.Fatalf("inconsistent IED name: IED=%q ConnectedAP=%q", doc.IED.Name, doc.Communication.SubNetwork.ConnectedAP.IEDName)
	}
	gse := doc.Communication.SubNetwork.ConnectedAP.GSE
	if gse.LDInst != "LD0" || gse.CBName != "Control" {
		t.Fatalf("unexpected GSE reference: %+v", gse)
	}
	params := make(map[string]string)
	for _, param := range gse.Address.Params {
		params[param.Type] = param.Value
	}
	if params["MAC-Address"] != "01-0C-CD-01-00-01" || params["APPID"] != "0001" || params["VLAN-ID"] != "064" {
		t.Fatalf("unexpected GSE address: %#v", params)
	}

	ld := doc.IED.AccessPoint.Server.LDevice
	if ld.Inst != "LD0" || ld.LDName != "IED1" {
		t.Fatalf("unexpected logical device: inst=%q ldName=%q", ld.Inst, ld.LDName)
	}
	if ld.LN0.DataSet.Name != "DataSet1" || len(ld.LN0.DataSet.FCDAs) != 4 {
		t.Fatalf("unexpected dataset: name=%q entries=%d", ld.LN0.DataSet.Name, len(ld.LN0.DataSet.FCDAs))
	}
	wantRefs := []struct {
		doName string
		daName string
		fc     string
	}{
		{"Signal1", "stVal", "ST"},
		{"Signal1", "q", "ST"},
		{"Signal2", "mag.f", "MX"},
		{"Signal2", "q", "MX"},
	}
	for i, want := range wantRefs {
		got := ld.LN0.DataSet.FCDAs[i]
		if got.DOName != want.doName || got.DAName != want.daName || got.FC != want.fc {
			t.Fatalf("FCDA #%d: got %+v, want %+v", i, got, want)
		}
	}
	control := ld.LN0.GSEControl
	if control.Name != "Control" || control.AppID != "CTRL1" || control.DatSet != "DataSet1" || control.ConfRev != 7 {
		t.Fatalf("unexpected GSEControl: %+v", control)
	}
}

func TestMarshalGoosePublisherRejectsReferenceMismatch(t *testing.T) {
	_, err := MarshalGoosePublisher(GoosePublisher{
		Name:    "PBMT",
		DstMAC:  "01:0c:cd:01:00:01",
		GocbRef: "IED1/LLN0$GO$Control",
		DatSet:  "IED2/LLN0$DataSet1",
		Dataset: []DatasetEntry{{Name: "Signal1.stVal", Type: "bool"}},
	})
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected logical-device mismatch, got %v", err)
	}
}

func TestMarshalGoosePublisherRejectsMixedObjectTypes(t *testing.T) {
	_, err := MarshalGoosePublisher(GoosePublisher{
		Name:    "PBMT",
		DstMAC:  "01:0c:cd:01:00:01",
		GocbRef: "IED1/LLN0$GO$Control",
		DatSet:  "IED1/LLN0$DataSet1",
		Dataset: []DatasetEntry{
			{Name: "Signal1.stVal", Type: "bool"},
			{Name: "Signal1.q", Type: "int"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "mixes incompatible types") {
		t.Fatalf("expected mixed-type error, got %v", err)
	}
}
