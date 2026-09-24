package presentation

import (
	"encoding/base64"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
	"pbmt/internal/config"
)

type mcpUIRuntime struct {
	control.Controller
	active bool
}

func (r *mcpUIRuntime) AgentActive() bool { return r.active }
func (*mcpUIRuntime) AcquireAgent() (control.Agent, error) {
	panic("factory stub should acquire control")
}

type mcpUIServer struct {
	runtime *mcpUIRuntime
	state   automation.ServerStatus
	stops   int
}

func (s *mcpUIServer) Status() automation.ServerStatus { return s.state }
func (s *mcpUIServer) Stop() error {
	s.stops++
	s.state.Running = false
	s.runtime.active = false
	return nil
}

func TestMCPPanelUsesEditableTokenAndActualLifecycle(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	runtime := &mcpUIRuntime{}
	server := &mcpUIServer{runtime: runtime}
	s := &uiState{app: app, window: app.NewWindow("MCP"), configPath: "test-project", cfg: &config.Config{}, runtime: runtime}
	var received automation.ServerOptions
	s.deps.StartMCP = func(root string, cfg *config.Config, p control.AgentProvider, o automation.ServerOptions) (automation.Server, error) {
		if root != s.configPath || cfg != s.cfg || p != runtime {
			t.Fatal("wrong project passed to server")
		}
		received = o
		runtime.active = true
		server.state = automation.ServerStatus{Running: true, Address: "http://" + o.Address + "/mcp"}
		return server, nil
	}
	actions := newMCPCatalogActions(func() {})
	panel, update := s.mcpServerPanel(actions)
	form := panel.(*fyne.Container).Objects[0].(*widget.Form)
	if form.Items[0].Text != "Server IP" || form.Items[1].Text != "Server Port" || form.Items[2].Text != "Token" {
		t.Fatal("network labels differ")
	}
	ip := form.Items[0].Widget.(*widget.Entry)
	port := form.Items[1].Widget.(*widget.Entry)
	tokenRow := form.Items[2].Widget.(*fyne.Container)
	var token *widget.Entry
	var copyToken *widget.Button
	var regenToken *widget.Button
	for _, object := range tokenRow.Objects {
		switch v := object.(type) {
		case *widget.Entry:
			token = v
		case *fyne.Container:
			if len(v.Objects) != 2 {
				t.Fatal("expected Copy and Regen buttons")
			}
			copyToken = v.Objects[0].(*widget.Button)
			regenToken = v.Objects[1].(*widget.Button)
		}
	}
	if token == nil || token.Disabled() || token.Text != "1234qwerASDF!" {
		t.Fatal("token is missing or cannot be edited")
	}
	if copyToken == nil || copyToken.Text != "Copy" || regenToken == nil || regenToken.Text != "Regen" || regenToken.Disabled() {
		t.Fatal("missing token actions")
	}
	test.Tap(regenToken)
	generated := token.Text
	if len(generated) != 12 || generated == "1234qwerASDF!" || s.mcpToken != generated {
		t.Fatal("regeneration did not update the token")
	}
	test.Tap(copyToken)
	if app.Clipboard().Content() != generated {
		t.Fatal("generated token copy failed")
	}
	token.SetText("manually-entered-shared-token-123456")
	ip.SetText("192.168.5.1")
	port.SetText("9876")
	test.Tap(copyToken)
	if s.app.Clipboard().Content() != token.Text {
		t.Fatal("token copy failed")
	}
	start := actions.Objects[1].(*widget.Button)
	stop := actions.Objects[2].(*widget.Button)
	if start.Disabled() || !stop.Disabled() {
		t.Fatal("wrong initial button states")
	}
	test.Tap(start)
	if received.Address != "192.168.5.1:9876" || received.Token != token.Text || !s.mcpActive() {
		t.Fatal("edited connection settings ignored")
	}
	if !start.Disabled() || stop.Disabled() || !token.Disabled() || !ip.Disabled() || !port.Disabled() || !regenToken.Disabled() {
		t.Fatal("active settings not locked")
	}
	// A queued callback must not rotate credentials after control was acquired.
	regenToken.OnTapped()
	if token.Text != received.Token || s.mcpToken != received.Token {
		t.Fatal("active token was regenerated")
	}
	// No protocol controls are constructed in Manual while MCP owns the runtime.
	_, cancel := s.manualModuleViewForWindow(moduleGoosePub, s.window)
	cancel()
	test.Tap(stop)
	if server.stops != 1 || s.mcpActive() || token.Disabled() || start.Disabled() || regenToken.Disabled() {
		t.Fatal("stop did not restore Manual/settings")
	}
	test.Tap(start)
	server.state.Running = false
	server.state.Error = "heartbeat lost"
	runtime.active = false
	update()
	if s.mcpActive() || token.Disabled() || !stop.Disabled() || regenToken.Disabled() {
		t.Fatal("automatic stop left controls locked")
	}
	// A view rebuild retains an edited token instead of silently generating another.
	_, _ = s.mcpServerPanel(newMCPCatalogActions(func() {}))
	if s.mcpToken != "manually-entered-shared-token-123456" {
		t.Fatal("edited token was lost")
	}
	s.stopMCP()
}

func TestGenerateMCPToken(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token, err := generateMCPToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 12 || seen[token] {
			t.Fatal("expected a fresh twelve-character token")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(decoded) != 9 {
			t.Fatal("expected nine random bytes in URL-safe encoding")
		}
		seen[token] = true
	}
}
