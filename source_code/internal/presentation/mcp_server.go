package presentation

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
)

func (s *uiState) mcpActive() bool {
	if s.mcpServer != nil && s.mcpServer.Status().Running {
		return true
	}
	provider, ok := s.runtime.(control.AgentProvider)
	return ok && provider.AgentActive()
}

// View disposal cancels pending auto-send callbacks. The controller also fences
// all manual writes, including callbacks queued before the mode changed.
func (s *uiState) refreshControlOwner() {
	s.disposeModuleViews()
	if !s.mcpActive() && s.runtime != nil && s.cfg != nil {
		for _, stream := range s.cfg.SVPub.Streams {
			if snapshot, ok := s.runtime.SVPublisherSnapshot(stream.Name); ok && snapshot.Ready {
				loadSVPublisherSnapshot(s.svPublisherManualState(stream), snapshot)
			}
		}
	}
	if s.manualPane != nil {
		s.manualPane.Objects = []fyne.CanvasObject{s.manualPlaceholder()}
		s.manualPane.Refresh()
	}
	s.refreshDashboard()
}

func (s *uiState) stopMCP() {
	if s.mcpServer == nil {
		return
	}
	if err := s.mcpServer.Stop(); err != nil {
		slog.Error("stop MCP failed", "error", err)
		if s.window != nil {
			showError(err, s.window)
		}
	}
	s.mcpServer = nil
}

// The token stays in application memory, not in the public catalog or logs.
// Users may replace it while stopped and copy it to the remote client's config.
func (s *uiState) mcpServerPanel(actions *fyne.Container) (fyne.CanvasObject, func()) {
	ip := widget.NewEntry()
	port := widget.NewEntry()
	ip.SetText(s.app.Preferences().StringWithFallback("mcp.server_ip", "0.0.0.0"))
	port.SetText(s.app.Preferences().StringWithFallback("mcp.server_port", "8765"))
	if s.mcpToken == "" {
		s.mcpToken = "1234qwerASDF!"
	}
	token := widget.NewPasswordEntry()
	token.SetPlaceHolder("12+ characters")
	token.SetText(s.mcpToken)
	token.OnChanged = func(text string) { s.mcpToken = text }
	copyToken := widget.NewButton("Copy", func() { s.app.Clipboard().SetContent(token.Text) })
	regenToken := widget.NewButton("Regen", func() {
		if s.mcpActive() {
			return
		}
		value, err := generateMCPToken()
		if err != nil {
			showError(fmt.Errorf("generate MCP token: %w", err), s.window)
			return
		}
		token.SetText(value)
	})
	endpoint := widget.NewLabel("")
	endpoint.Wrapping = fyne.TextWrapWord
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	warning := widget.NewLabel("HTTP: trusted test LAN only. Change the shared default token outside an isolated test bench. For 0.0.0.0, connect using this PC's LAN IP. Token changes apply on Start MCP.")
	warning.Wrapping = fyne.TextWrapWord
	form := widget.NewForm(widget.NewFormItem("Server IP", ip), widget.NewFormItem("Server Port", port),
		widget.NewFormItem("Token", container.NewBorder(nil, nil, nil, container.NewHBox(copyToken, regenToken), token)))
	start := actions.Objects[1].(*widget.Button)
	stop := actions.Objects[2].(*widget.Button)
	lastActive := s.mcpActive()
	update := func() {
		active := s.mcpActive()
		if active != lastActive {
			lastActive = active
			s.refreshControlOwner()
		}
		_, supported := s.runtime.(control.AgentProvider)
		setButtonEnabled(start, !active && supported && s.deps.StartMCP != nil)
		setButtonEnabled(stop, active)
		setButtonEnabled(regenToken, !active)
		for _, entry := range []*widget.Entry{ip, port, token} {
			if active {
				entry.Disable()
			} else {
				entry.Enable()
			}
		}
		state := "Stopped — Manual control"
		if s.mcpServer != nil {
			st := s.mcpServer.Status()
			if st.Running {
				state = "Running — waiting for agent; Manual locked"
				if st.Connected {
					state = "Running — agent connected; Manual locked"
				}
			}
			if st.Error != "" {
				state += "\n" + st.Error
			}
			endpoint.SetText(st.Address)
		} else {
			endpoint.SetText("Endpoint: http://" + net.JoinHostPort(strings.TrimSpace(ip.Text), strings.TrimSpace(port.Text)) + "/mcp")
		}
		status.SetText(state)
	}
	start.OnTapped = func() {
		if s.mcpActive() {
			return
		}
		provider, ok := s.runtime.(control.AgentProvider)
		if !ok || s.deps.StartMCP == nil {
			return
		}
		server, err := s.deps.StartMCP(s.configPath, s.cfg, provider, automation.ServerOptions{
			Address: net.JoinHostPort(strings.TrimSpace(ip.Text), strings.TrimSpace(port.Text)), Token: token.Text,
		})
		if err != nil {
			slog.Error("start MCP failed", "error", err)
			showError(fmt.Errorf("start MCP: %w", err), s.window)
			return
		}
		s.mcpServer = server
		s.app.Preferences().SetString("mcp.server_ip", strings.TrimSpace(ip.Text))
		s.app.Preferences().SetString("mcp.server_port", strings.TrimSpace(port.Text))
		update()
	}
	stop.OnTapped = func() { s.stopMCP(); update() }
	update()
	return container.NewVBox(form, endpoint, status, warning), update
}

func generateMCPToken() (string, error) {
	// Nine random bytes encode to exactly twelve URL-safe ASCII characters.
	var data [9]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}
