package presentation

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"gopkg.in/yaml.v3"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
)

func subscriberICDFixture(t *testing.T) ([]byte, sclmodel.GooseStream) {
	t.Helper()
	m := &sclmodel.Model{Endpoint: sclmodel.Endpoint{IP: "10.10.5.22", Port: 102}, Devices: []sclmodel.LogicalDevice{{Name: "IEDLD0", Nodes: []sclmodel.LogicalNode{{Name: "LLN0", Groups: []*sclmodel.Type{{Name: "ST", Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "Ind1", Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "stVal", Kind: "BOOLEAN"}, {Name: "q", Kind: "BIT_STRING", Size: 13}}}}}}, DataSets: []sclmodel.DataSet{{Name: "events", Members: []string{"IEDLD0/LLN0.Ind1.stVal[ST]", "IEDLD0/LLN0.Ind1.q[ST]"}}}, GOOSE: []sclmodel.GooseControl{{Name: "events", DataSet: "IEDLD0/LLN0$events", GoID: "goose-events", MAC: "01-0C-CD-01-00-22", APPID: 3, ConfRev: 7}}}}}}}
	raw, _, err := sclmodel.ExportICD(m)
	if err != nil {
		t.Fatal(err)
	}
	streams, err := sclmodel.ParseGooseSCL(raw)
	if err != nil || len(streams) != 1 {
		t.Fatalf("fixture: %v", err)
	}
	g := streams[0]
	if g.FrameError != "" || g.SchemaError != "" {
		t.Fatalf("fixture errors: %+v", g)
	}
	return raw, g
}

func TestGooseImportOnlyChangesSubscriptionFields(t *testing.T) {
	_, g := subscriberICDFixture(t)
	node := mustDefaultConfigItemTemplate(t, moduleGooseSub)
	setMappingValue(node, "accept_test", scalarBool(true))
	setMappingValue(node, "allow_nonstandard", scalarBool(true))
	setMappingValue(node, "src_mac", scalarString("00:11:22:33:44:55"))
	g.VLANID = nil
	g.VLANPriority = nil
	if err := applyGooseImport(node, g); err != nil {
		t.Fatal(err)
	}
	var sub config.GooseSubscriber
	if err := node.Decode(&sub); err != nil {
		t.Fatal(err)
	}
	if sub.Name != "events" || sub.GocbRef != g.GocbRef || sub.DatSet != g.DatSet || uint16(sub.AppID) != 3 || *sub.ConfRev != 7 || sub.GoID != "goose-events" || sub.SrcMAC != "" || sub.VLANID != nil || sub.VLANPri != nil || sub.MatchAnyAppID {
		t.Fatalf("incorrect import: %+v", sub)
	}
	if !sub.AcceptTest || !sub.AllowNonStandard {
		t.Fatal("import changed reception policy")
	}
	if mappingValue(node, "dataset") != nil {
		t.Fatal("signal schema stored in YAML")
	}
	before, _ := yaml.Marshal(node)
	g.FrameError = "missing APPID"
	if applyGooseImport(node, g) == nil {
		t.Fatal("bad stream imported")
	}
	after, _ := yaml.Marshal(node)
	if string(before) != string(after) {
		t.Fatal("failed import partially modified settings")
	}
}

func TestGooseManualUsesICDNamesOnlyForMatchingFrames(t *testing.T) {
	_, g := subscriberICDFixture(t)
	g.VLANID = nil
	g.VLANPriority = nil
	entries, message := describedGooseEntries(&g, nil)
	if !strings.Contains(message, "Waiting") || entries[0].Name != "IEDLD0/LLN0.Ind1.stVal" || entries[0].Summary != "—" {
		t.Fatal("incorrect pending signals")
	}
	mac, _ := net.ParseMAC(g.DstMAC)
	p := &goose.PDU{DstMAC: mac, GocbRef: g.GocbRef, DatSet: g.DatSet, GoID: g.GoID, AppID: g.AppID, ConfRev: g.ConfRev, NumDatSetEntries: 2, AllData: []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}, {Type: goose.DataTypeBitString, BitLength: 13, Bytes: []byte{0, 0}}}}
	entries, message = describedGooseEntries(&g, p)
	if entries[0].Name != g.Entries[0].Name || entries[0].Summary != "true" || !strings.Contains(message, "matches") {
		t.Fatalf("labels not applied: %+v %s", entries, message)
	}
	p.ConfRev++
	entries, message = describedGooseEntries(&g, p)
	if entries[0].Name != "Entry1" || entries[0].Summary != "true" || !strings.Contains(message, "mismatch") {
		t.Fatal("mismatched frame received misleading names")
	}
	if p.AllData[0].Bool != true {
		t.Fatal("received value mutated")
	}
}

func TestGooseProjectICDIsReadOnlyAndAmbiguityDisablesLabels(t *testing.T) {
	raw, g := subscriberICDFixture(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "10.10.5.22.icd"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	streams, message := projectGooseDescriptions(root)
	if len(streams) != 1 {
		t.Fatal(message)
	}
	sub := config.GooseSubscriber{GocbRef: g.GocbRef, DatSet: g.DatSet, AppID: config.HexU16(g.AppID)}
	if model, _ := gooseDescription(sub, streams); model == nil {
		t.Fatal("description not selected")
	}
	if model, _ := gooseDescription(sub, append(streams, streams...)); model != nil {
		t.Fatal("ambiguous stream selected")
	}
	if err := os.WriteFile(filepath.Join(root, "copy.icd"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if streams, message := projectGooseDescriptions(root); len(streams) != 0 || !strings.Contains(message, "Multiple") {
		t.Fatal("ambiguous project ICD selected")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatal("model read created extra files")
	}
}

func TestGooseManualInitiallyShowsExpectedSignals(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	raw, g := subscriberICDFixture(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "10.10.5.22.icd"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := &uiState{app: app, window: app.NewWindow("test"), configPath: root, cfg: &config.Config{GooseSub: config.GooseSubCfg{Subscriptions: []config.GooseSubscriber{{Name: "events", GocbRef: g.GocbRef, DatSet: g.DatSet, AppID: config.HexU16(g.AppID)}}}}}
	view, cancel := s.gooseSubscriberControl()
	defer cancel()
	s.window.SetContent(view)
	var titles []string
	var visit func(fyne.CanvasObject)
	visit = func(obj fyne.CanvasObject) {
		switch x := obj.(type) {
		case *fyne.Container:
			for _, c := range x.Objects {
				visit(c)
			}
		case *container.Scroll:
			visit(x.Content)
		case *widget.Card:
			visit(x.Content)
		case *widget.Accordion:
			for _, item := range x.Items {
				titles = append(titles, item.Title)
			}
		}
	}
	visit(view)
	if len(titles) != 2 || titles[0] != "IEDLD0/LLN0.Ind1.stVal: —" {
		t.Fatalf("pending signal list: %v", titles)
	}
}
