package presentation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/catalog"
	"pbmt/internal/config"
	"pbmt/profiles"
)

func TestMCPCatalogActionsShareOneRowAndDoNotSimulateServer(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	refreshes := 0
	actions := newMCPCatalogActions(func() { refreshes++ })
	actions.Resize(fyne.NewSize(900, 48))
	if len(actions.Objects) != 3 {
		t.Fatalf("expected three buttons, got %d", len(actions.Objects))
	}
	for i, name := range []string{"Refresh Catalog", "Start MCP", "Stop MCP"} {
		button, ok := actions.Objects[i].(*widget.Button)
		if !ok || button.Text != name {
			t.Fatalf("unexpected action at index %d", i)
		}
		if button.Disabled() != (i != 0) {
			t.Fatalf("unexpected enabled state for %s", name)
		}
		if i > 0 && (button.Position().Y != actions.Objects[0].Position().Y || button.Position().X <= actions.Objects[i-1].Position().X) {
			t.Fatal("actions are not arranged left to right in one row")
		}
	}
	test.Tap(actions.Objects[0].(*widget.Button))
	if refreshes != 1 {
		t.Fatal("Refresh Catalog callback was not called")
	}
}

func TestMCPCatalogPageGeneratesProjectFileWithoutRuntime(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	root := filepath.Join(t.TempDir(), "project")
	if err := profiles.CreateProject(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	s := &uiState{app: app, window: app.NewWindow("catalog"), configPath: root, cfg: cfg}
	page := s.mcpCatalogPage()
	defer s.catalogView.dispose()
	s.window.SetContent(page)
	s.window.Resize(fyne.NewSize(1140, 640))
	if page.MinSize().Width > 1140 {
		t.Fatalf("MCP network panel overflows the default window: %v", page.MinSize())
	}
	raw, err := os.ReadFile(filepath.Join(root, catalog.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var c catalog.Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Modules) != 4 || len(c.Modules[3].Streams[0].Signals) != 8 || s.runtime != nil {
		t.Fatal("catalog generation touched runtime or lost channels")
	}
	path := filepath.Join(root, "cfg", "sv_pub.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oldName := cfg.SVPub.Streams[0].Name
	after := strings.Replace(string(before), "name: "+oldName, "name: edited", 1)
	if err := os.WriteFile(path, []byte(after), 0600); err != nil {
		t.Fatal(err)
	}
	s.catalogView.refresh(false) // The same callback is used by automatic polling.
	raw, _ = os.ReadFile(filepath.Join(root, catalog.FileName))
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if c.ConfigurationState != "differs_from_applied" || c.Modules[3].Streams[0].Name != "edited" || cfg.SVPub.Streams[0].Name != oldName {
		t.Fatal("refresh failed to distinguish saved/applied settings")
	}
	s.catalogView.dispose()
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	s.catalogView.refresh(true)
	unchanged, _ := os.ReadFile(filepath.Join(root, catalog.FileName))
	if string(raw) != string(unchanged) {
		t.Fatal("disposed view updated old project")
	}
}

func TestMCPCatalogPageWithoutProjectDoesNotWrite(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	root := t.TempDir()
	s := &uiState{app: app, window: app.NewWindow("catalog"), configPath: root}
	s.mcpCatalogPage()
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("catalog written without project configuration")
	}
}
