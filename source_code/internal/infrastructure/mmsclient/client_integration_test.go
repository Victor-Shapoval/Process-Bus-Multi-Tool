//go:build mmsintegration

package mmsclient

import (
	"context"
	"encoding/xml"
	"net"
	"pbmt/internal/application/sclmodel"
	"strings"
	"testing"
	"time"
)

// This test binds only loopback and never enables GOOSE/SV publication.
func TestDiscoverReadExportAndReconnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := startFixture(port)
	defer server.close()
	if !server.running() {
		t.Fatal("fixture failed to start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint := sclmodel.Endpoint{IP: "127.0.0.1", Port: port}
	client, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	model, err := client.Discover(ctx, nil)
	client.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Devices) != 1 || model.Devices[0].Name != "TestIEDLD0" {
		t.Fatalf("unexpected model: %+v", model)
	}
	if len(model.Warnings) != 0 {
		t.Fatalf("discovery warnings: %v", model.Warnings)
	}
	zero := model.Devices[0].Nodes[1]
	if zero.Name != "LLN0" {
		t.Fatalf("node order: %+v", model.Devices[0].Nodes)
	}
	if len(zero.DataSets) != 1 || len(zero.DataSets[0].Members) != 2 {
		t.Fatalf("dataset: %+v", zero.DataSets)
	}
	if len(zero.GOOSE) != 1 || zero.GOOSE[0].MAC != "01-0C-CD-01-00-02" || zero.GOOSE[0].APPID != 0x1001 {
		t.Fatalf("GoCB: %+v", zero.GOOSE)
	}
	if zero.Settings == nil || zero.Settings.NumGroups != 4 || zero.Settings.ActiveGroup != 2 {
		t.Fatalf("SGCB: %+v", zero.Settings)
	}
	if len(zero.Reports) != 2 {
		t.Fatalf("RCBs: %+v", zero.Reports)
	}
	for _, r := range zero.Reports {
		if !r.Buffered {
			if r.ReportID != "TestURCB" || r.ConfRev != 9 || r.BufferTime != 25 || r.IntegrityPeriod != 3000 || !r.Triggers.DataChange || !r.Triggers.Integrity || !r.Triggers.GI || r.Triggers.QualityChange || r.Triggers.DataUpdate || !r.Options.SeqNum || !r.Options.DataSet || !r.Options.ConfRev || r.Options.EntryID {
				t.Fatalf("URCB: %+v", r)
			}
		} else {
			if r.ReportID != "TestBRCB" || r.ConfRev != 10 || r.BufferTime != 50 || r.IntegrityPeriod != 5000 || !r.Triggers.QualityChange || !r.Triggers.DataUpdate || r.Triggers.DataChange || !r.Options.TimeStamp || !r.Options.ReasonCode || !r.Options.DataRef || !r.Options.BufOvfl || !r.Options.EntryID || r.Options.SeqNum {
				t.Fatalf("BRCB: %+v", r)
			}
		}
	}
	tree, err := sclmodel.BuildTree(model)
	if err != nil {
		t.Fatal(err)
	}
	var state, analog sclmodel.Target
	for _, n := range tree.Nodes {
		if n.Target != nil {
			if n.Target.Item == "GGIO1$ST$Ind1$stVal" {
				state = *n.Target
			}
			if n.Target.Item == "GGIO1$MX$AnIn1$mag$f" {
				analog = *n.Target
			}
		}
	}
	if state.ID == "" || analog.ID == "" {
		t.Fatal("missing leaf targets")
	}
	client, err = Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	values, err := client.Read(ctx, []sclmodel.Target{state, analog})
	if err != nil {
		t.Fatal(err)
	}
	if values[state.ID].Value != "true" || !strings.Contains(values[analog.ID].Value, "12.5") {
		t.Fatalf("readings: %+v", values)
	}
	server.set(false)
	values, err = client.Read(ctx, []sclmodel.Target{state})
	if err != nil || values[state.ID].Value != "false" {
		t.Fatalf("changed reading: %+v %v", values, err)
	}
	bytes, warnings, err := sclmodel.ExportICD(model)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		XMLName xml.Name
		IED     struct {
			Name string `xml:"name,attr"`
		}
	}
	if err := xml.Unmarshal(bytes, &document); err != nil {
		t.Fatal(err)
	}
	if document.XMLName.Local != "SCL" || document.IED.Name != "DiscoveredIED" {
		t.Fatalf("export: %+v", document)
	}
	if !strings.Contains(string(bytes), `name="Ind1"`) || !strings.Contains(string(bytes), `name="gcbEvents"`) {
		t.Fatalf("missing standard SCL declarations; warnings=%v", warnings)
	}
	if strings.Count(string(bytes), "<ReportControl ") != 2 || !strings.Contains(string(bytes), `<SettingControl numOfSGs="4" actSG="2"`) {
		t.Fatalf("controls missing in export; warnings=%v", warnings)
	}
	if len(warnings) != 1 {
		t.Fatalf("unexpected export omissions: %v", warnings)
	}
	// Reconnect/read targets must also work when reconstructed solely from ICD.
	restored, err := sclmodel.ImportICD(bytes)
	if err != nil {
		t.Fatal(err)
	}
	importedTree, err := sclmodel.BuildTree(restored)
	if err != nil {
		t.Fatal(err)
	}
	var importedTargets []sclmodel.Target
	for _, n := range importedTree.Nodes {
		if n.Target != nil && (n.Target.Item == state.Item || n.Target.Item == analog.Item) {
			importedTargets = append(importedTargets, *n.Target)
		}
	}
	if len(importedTargets) != 2 {
		t.Fatal("ICD lost monitored attributes")
	}
	values, err = client.Read(ctx, importedTargets)
	if err != nil || values[state.ID].Value != "false" || !strings.Contains(values[analog.ID].Value, "12.5") {
		t.Fatalf("ICD-derived MMS readings: %+v %v", values, err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := client.Read(canceled, []sclmodel.Target{state}); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
