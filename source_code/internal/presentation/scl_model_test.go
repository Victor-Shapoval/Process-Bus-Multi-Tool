package presentation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"pbmt/internal/application/sclmodel"
)

func TestSCLModelButtonsEndpointBindingAndStaleValues(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	owner := &uiState{app: app, window: app.NewWindow("test"), configPath: t.TempDir(), deps: Dependencies{DialModel: func(context.Context, sclmodel.Endpoint) (sclmodel.Client, error) { return nil, fmt.Errorf("test only") }}}
	page := owner.sclModelPage()
	owner.window.SetContent(page)
	p := owner.sclModelView
	defer p.dispose()
	if !p.connect.Disabled() || !p.save.Disabled() {
		t.Fatal("connect/save enabled before model read")
	}
	p.model = &sclmodel.Model{Endpoint: sclmodel.Endpoint{IP: "127.0.0.1", Port: 102}}
	p.ip.SetText("127.0.0.1")
	p.buttons()
	if p.connect.Disabled() || p.save.Disabled() {
		t.Fatal("read model did not enable actions")
	}
	p.ip.SetText("127.0.0.2")
	if !p.connect.Disabled() {
		t.Fatal("new device accepted for old model")
	}
	p.ip.SetText("127.0.0.1")
	p.readings["signal"] = sclmodel.Reading{Value: "true", ReadAt: time.Now()}
	if !strings.Contains(p.valueText("signal"), "stale") {
		t.Fatal("disconnected value presented as current")
	}
	p.connected = true
	if p.valueText("signal") != "true" {
		t.Fatal("fresh connected value marked stale")
	}
	p.readings["signal"] = sclmodel.Reading{Value: "true", ReadAt: time.Now().Add(-10 * time.Second)}
	if !strings.Contains(p.valueText("signal"), "stale") {
		t.Fatal("expired value presented as current")
	}
	canceled := false
	p.cancel = func() { canceled = true }
	p.dispose()
	if !canceled {
		t.Fatal("view disposal did not cancel worker")
	}
}

func TestSCLModelFileNameUsesDeviceIP(t *testing.T) {
	for input, want := range map[string]string{"10.10.5.22": "10.10.5.22.icd", " 192.168.1.1 ": "192.168.1.1.icd", "2001:db8::1": "2001_db8__1.icd", "invalid/path": "DiscoveredIED.icd"} {
		if got := sclModelFileName(input); got != want {
			t.Errorf("%q: %q, want %q", input, got, want)
		}
	}
}

func TestSCLModelICDRestoresOfflineOnly(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	root := t.TempDir()
	model := &sclmodel.Model{Endpoint: sclmodel.Endpoint{IP: "127.0.0.1", Port: 8102}, ReadAt: time.Now(), Devices: []sclmodel.LogicalDevice{{Name: "IEDLD0", Nodes: []sclmodel.LogicalNode{{Name: "LLN0"}}}}}
	raw, _, err := sclmodel.ExportICD(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "127.0.0.1.icd"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	owner := &uiState{app: app, window: app.NewWindow("test"), configPath: root, deps: Dependencies{DialModel: func(context.Context, sclmodel.Endpoint) (sclmodel.Client, error) {
		t.Fatal("ICD loading must not connect")
		return nil, nil
	}}}
	owner.sclModelPage()
	p := owner.sclModelView
	defer p.dispose()
	if p.model == nil || p.port.Text != "102" || p.ip.Text != "127.0.0.1" || p.connected {
		t.Fatal("ICD model not restored offline")
	}
	if len(p.readings) != 0 {
		t.Fatal("ICD restored live values")
	}
	p.port.SetText("8102")
	if !p.matches() || p.connect.Disabled() {
		t.Fatal("cannot choose custom port for an ICD model")
	}
	if _, err := os.Stat(filepath.Join(root, "scl_model.json")); !os.IsNotExist(err) {
		t.Fatal("JSON cache created")
	}
}

func TestSCLModelMultipleICDsAreNotAutomaticallySelectedAndInvalidICDRetainsTree(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	root := t.TempDir()
	model := &sclmodel.Model{Endpoint: sclmodel.Endpoint{IP: "127.0.0.1", Port: 102}, Devices: []sclmodel.LogicalDevice{{Name: "IEDLD0", Nodes: []sclmodel.LogicalNode{{Name: "LLN0"}}}}}
	raw, _, err := sclmodel.ExportICD(model)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"a.icd": raw, "b.ICD": []byte("broken xml"), "scl_model.json": []byte(`{}`)} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	owner := &uiState{app: app, window: app.NewWindow("test"), configPath: root, deps: Dependencies{DialModel: func(context.Context, sclmodel.Endpoint) (sclmodel.Client, error) {
		t.Fatal("unexpected network access")
		return nil, nil
	}}}
	owner.sclModelPage()
	p := owner.sclModelView
	defer p.dispose()
	if p.model != nil || !strings.Contains(p.status.Text, "Multiple ICD") {
		t.Fatal("ambiguous ICD chosen automatically")
	}
	p.loadICD("a.icd")
	previous := p.model
	if previous == nil {
		t.Fatal("selected ICD not loaded")
	}
	p.loadICD("b.ICD")
	if p.model != previous || !strings.Contains(p.status.Text, "Cannot load") {
		t.Fatal("invalid ICD replaced model or error not shown")
	}
}

func TestSCLModelPollsOnlyVisibleExpandedLeaves(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	owner := &uiState{app: app, window: app.NewWindow("test"), configPath: t.TempDir()}
	page := owner.sclModelPage()
	p := owner.sclModelView
	defer p.dispose()
	var attrs []*sclmodel.Type
	for i := 0; i < 100; i++ {
		attrs = append(attrs, &sclmodel.Type{Name: fmt.Sprintf("value%d", i), Kind: "BOOLEAN"})
	}
	model := &sclmodel.Model{Devices: []sclmodel.LogicalDevice{{Name: "IEDLD0", Nodes: []sclmodel.LogicalNode{{Name: "LLN0", Groups: []*sclmodel.Type{{Name: "ST", Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "State", Kind: "STRUCTURE", Children: attrs}}}}}}}}}
	data, err := sclmodel.BuildTree(model)
	if err != nil {
		t.Fatal(err)
	}
	p.data = data
	owner.window.SetContent(page)
	owner.window.Resize(fyne.NewSize(1100, 400))
	owner.window.Show()
	p.tree.OpenAllBranches()
	p.tree.Refresh()
	owner.window.Canvas().Capture()
	targets := p.visibleTargets()
	if len(targets) == 0 || len(targets) >= 100 {
		t.Fatalf("visible subset: got %d targets", len(targets))
	}
	p.tree.CloseAllBranches()
	p.tree.Refresh()
	if len(p.visibleTargets()) != 0 {
		t.Fatal("collapsed leaves are still polled")
	}
}
