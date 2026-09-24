package presentation

import (
	"fmt"
	"image/color"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	pbmtruntime "pbmt/internal/application/control"
	"pbmt/internal/config"
)

type manualPopOutView struct {
	window fyne.Window
	cancel func()
	once   sync.Once
}

func (v *manualPopOutView) dispose() {
	if v == nil {
		return
	}
	v.once.Do(func() {
		if v.cancel != nil {
			v.cancel()
		}
	})
}

func (s *uiState) refreshDashboard() {
	if s.dashboard == nil {
		return
	}
	s.dashboard.Objects = nil

	ids := []moduleID{
		moduleGooseSub,
		moduleGoosePub,
		moduleSVSub,
		moduleSVPub,
		modulePTPClient,
		modulePTPServer,
	}

	var cards []fyne.CanvasObject
	for _, id := range ids {
		if !s.moduleEnabled(id) {
			continue
		}
		cards = append(cards, s.dashboardCard(id))
	}
	if len(cards) == 0 {
		empty := widget.NewLabel("No configured modules in the active profile.")
		empty.Alignment = fyne.TextAlignCenter
		s.dashboard.Add(widget.NewCard("Manual", "", container.NewPadded(empty)))
		s.dashboard.Refresh()
		return
	}
	s.dashboard.Add(container.NewVBox(cards...))
	s.dashboard.Refresh()
}

func (s *uiState) dashboardCard(id moduleID) fyne.CanvasObject {
	status := s.moduleStatus(id)
	bg := canvas.NewRectangle(s.statusColor(status))
	bg.SetMinSize(fyne.NewSize(220, 118))

	title := widget.NewButtonWithIcon(moduleTitle(id), moduleIcon(id), func() {
		s.showManualModule(id)
	})
	title.Importance = widget.HighImportance

	bodyText := moduleSubtitle(id) + "\nStatus: " + status
	body := widget.NewLabel(bodyText)
	body.Wrapping = fyne.TextWrapWord

	popOut := widget.NewButtonWithIcon("", theme.ViewRestoreIcon(), func() {
		s.openManualModuleWindow(id)
	})
	popOut.Importance = widget.LowImportance
	top := container.NewBorder(nil, nil, nil, popOut, title)
	bodyTap := container.NewStack(body, newTapArea(func() {
		s.showManualModule(id)
	}))
	return container.NewStack(bg, container.NewPadded(container.NewVBox(top, bodyTap)))
}

type tapArea struct {
	widget.BaseWidget
	onTapped func()
}

func newTapArea(onTapped func()) *tapArea {
	t := &tapArea{onTapped: onTapped}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tapArea) Tapped(*fyne.PointEvent) {
	if t.onTapped != nil {
		t.onTapped()
	}
}

func (t *tapArea) CreateRenderer() fyne.WidgetRenderer {
	r := canvas.NewRectangle(color.Transparent)
	return widget.NewSimpleRenderer(r)
}

func (s *uiState) manualPlaceholder() fyne.CanvasObject {
	msg := widget.NewLabel("Select a module on the left to open manual controls.")
	if s.mcpActive() {
		msg.SetText("MCP owns control. Stop MCP to return to Manual. PTP remains unchanged.")
	}
	msg.Alignment = fyne.TextAlignCenter
	msg.Wrapping = fyne.TextWrapWord
	return widget.NewCard("Manual", "", container.NewPadded(msg))
}

func (s *uiState) showManualModule(id moduleID) {
	if time.Now().Before(s.suppressManualOpen) {
		return
	}
	if existing := s.manualPopOutWindows[id]; existing != nil {
		existing.window.RequestFocus()
		return
	}
	if s.manualPane == nil {
		return
	}
	if s.manualStop != nil {
		s.manualStop()
		s.manualStop = nil
	}
	content, cancel := s.manualModuleView(id)
	s.manualStop = cancel
	s.manualOpenModule = id
	s.manualPane.Objects = []fyne.CanvasObject{content}
	s.manualPane.Refresh()
}

func (s *uiState) openManualModuleWindow(id moduleID) {
	s.suppressManualOpen = time.Now().Add(400 * time.Millisecond)
	if existing := s.manualPopOutWindows[id]; existing != nil {
		existing.window.RequestFocus()
		return
	}
	if s.manualOpenModule == id && s.manualPane != nil {
		if s.manualStop != nil {
			s.manualStop()
			s.manualStop = nil
		}
		s.manualOpenModule = ""
		s.manualPane.Objects = []fyne.CanvasObject{s.manualPlaceholder()}
		s.manualPane.Refresh()
	}
	w := s.app.NewWindow(moduleTitle(id))
	w.SetIcon(pbmtIcon)
	content, cancel := s.manualModuleViewForWindow(id, w)
	w.SetContent(content)
	w.Resize(fyne.NewSize(920, 650))
	view := &manualPopOutView{window: w, cancel: cancel}
	s.manualPopOutWindows[id] = view
	w.SetCloseIntercept(func() {
		s.closeManualPopOut(id, view)
	})
	w.SetOnClosed(func() {
		view.dispose()
		if s.manualPopOutWindows[id] == view {
			delete(s.manualPopOutWindows, id)
		}
	})
	w.Show()
}

func (s *uiState) closeManualPopOut(id moduleID, view *manualPopOutView) {
	if view == nil {
		return
	}
	view.dispose()
	if s.manualPopOutWindows[id] == view {
		delete(s.manualPopOutWindows, id)
	}
	view.window.SetCloseIntercept(nil)
	view.window.Close()
}

func (s *uiState) manualModuleView(id moduleID) (fyne.CanvasObject, func()) {
	return s.manualModuleViewForWindow(id, s.window)
}

func (s *uiState) manualModuleViewForWindow(id moduleID, owner fyne.Window) (fyne.CanvasObject, func()) {
	if s.mcpActive() {
		return s.manualPlaceholder(), func() {}
	}
	content, cancelContent, afterStart := s.moduleWindowContent(id, owner)
	_, supported := runtimeModuleID(id)
	var updateButtons func()

	start := widget.NewButtonWithIcon("Start Module", theme.MediaPlayIcon(), func() {
		if err := s.startModule(id); err != nil {
			updateButtons()
			showError(err, owner)
			return
		}
		if afterStart != nil {
			afterStart()
		}
		updateButtons()
	})
	stop := widget.NewButtonWithIcon("Stop Module", theme.MediaStopIcon(), func() {
		if err := s.stopModule(id); err != nil {
			updateButtons()
			showError(err, owner)
			return
		}
		updateButtons()
	})
	updateButtons = func() {
		state := manualModuleActionState(s.moduleStatus(id), supported)
		setButtonEnabled(start, state.startEnabled)
		setButtonEnabled(stop, state.stopEnabled)
	}
	updateButtons()

	cancelStatus := func() {}
	if supported {
		done := make(chan struct{})
		var cancelOnce sync.Once
		cancelStatus = func() {
			cancelOnce.Do(func() { close(done) })
		}
		go func() {
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()
			lastStatus := s.moduleStatus(id)
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					status := s.moduleStatus(id)
					if status != lastStatus {
						lastStatus = status
						fyne.Do(updateButtons)
					}
				}
			}
		}()
	}
	cancel := func() {
		cancelStatus()
		if cancelContent != nil {
			cancelContent()
		}
	}
	actions := container.NewPadded(container.NewHBox(start, stop))
	return container.NewBorder(nil, actions, nil, nil, content), cancel
}

type manualModuleButtons struct {
	startEnabled bool
	stopEnabled  bool
}

func manualModuleActionState(status string, supported bool) manualModuleButtons {
	if !supported {
		return manualModuleButtons{}
	}
	running := status == "Running"
	return manualModuleButtons{startEnabled: !running, stopEnabled: running}
}

func setButtonEnabled(button *widget.Button, enabled bool) {
	if enabled {
		button.Enable()
	} else {
		button.Disable()
	}
}

func (s *uiState) startModule(id moduleID) error {
	if rtID, ok := runtimeModuleID(id); ok {
		if s.runtime == nil {
			s.setModuleStatus(id, "Error")
			return fmt.Errorf("runtime is not initialized")
		}
		if err := s.runtime.Start(rtID); err != nil {
			s.refreshDashboard()
			return err
		}
		s.refreshDashboard()
		return nil
	}
	s.setModuleStatus(id, "Error")
	return fmt.Errorf("%s runtime start is not implemented in the GUI supervisor", moduleTitle(id))
}

func moduleConfigFile(id moduleID) string {
	switch id {
	case moduleGooseSub:
		return "goose_sub.yaml"
	case moduleGoosePub:
		return "goose_pub.yaml"
	case moduleSVSub:
		return "sv_sub.yaml"
	case moduleSVPub:
		return "sv_pub.yaml"
	case modulePTPClient:
		return "ptp_client.yaml"
	case modulePTPServer:
		return "ptp_server.yaml"
	default:
		return ""
	}
}

func (s *uiState) projectConfigDir() string {
	dir, err := config.ConfigDir(s.configPath)
	if err != nil {
		return filepath.Join(s.configPath, "cfg")
	}
	return dir
}

func (s *uiState) moduleEnabled(id moduleID) bool {
	if s.cfg == nil {
		return false
	}
	switch id {
	case moduleGooseSub:
		return s.cfg.GooseSub.Enabled
	case moduleGoosePub:
		return s.cfg.GoosePub.Enabled
	case moduleSVSub:
		return s.cfg.SVSub.Enabled
	case moduleSVPub:
		return s.cfg.SVPub.Enabled
	case modulePTPClient:
		return s.cfg.PTPClient.Enabled
	case modulePTPServer:
		return s.cfg.PTPServer.Enabled
	default:
		return false
	}
}

func (s *uiState) loadProject(path string) error {
	path = projectRootPath(path)
	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("project load failed", "path", path, "error", err)
		s.statusBar.SetText("Project load failed")
		return fmt.Errorf("selected folder is not a valid project: %w", err)
	}
	logPath := filepath.Join(path, "project.log")
	if s.configPath != "" && s.configPath != path {
		slog.Info("project switch requested", "from", s.configPath, "to", path)
	}
	closePrevious := func() {
		s.stopMCP()
		if s.catalogView != nil {
			s.catalogView.dispose()
			s.catalogView = nil
		}
		if s.sclModelView != nil {
			s.sclModelView.dispose()
			s.sclModelView = nil
		}
		s.disposeModuleViews()
		if s.runtime != nil {
			if err := s.runtime.Close(); err != nil {
				slog.Error("project shutdown failed", "path", s.configPath, "error", err)
			}
			s.runtime = nil
		}
		if s.configPath != "" {
			slog.Info("project closed", "path", s.configPath, "reason", "project_switch")
		}
	}
	if s.logLevel != nil {
		if err := s.logLevel.SetOutputFile(logPath, closePrevious); err != nil {
			slog.Error("project log switch failed", "path", logPath, "error", err)
			s.statusBar.SetText("Project load failed")
			return fmt.Errorf("configure project log: %w", err)
		}
	} else {
		closePrevious()
	}
	s.configPath = path
	slog.Info("project loaded", "path", path, "log", logPath)
	s.activateProjectConfig(cfg)
	s.statusBar.SetText("Project loaded: " + path)
	return nil
}

func projectRootPath(path string) string {
	path = filepath.Clean(path)
	if filepath.Base(path) == "cfg" {
		return filepath.Dir(path)
	}
	return path
}

func (s *uiState) activateProjectConfig(cfg *config.Config) {
	if s.mcpActive() {
		showError(fmt.Errorf("stop MCP before applying configuration"), s.window)
		return
	}
	mainTabIndex := selectedAppTabIndex(s.mainTabs)
	configurationTabIndex := selectedAppTabIndex(s.configurationTabs)
	s.disposeModuleViews()
	s.cfg = cfg
	if s.runtime == nil {
		s.runtime = s.deps.NewController(cfg)
	} else {
		s.runtime.UpdateConfig(cfg)
	}
	slog.Info("project configuration applied", "path", s.configPath)
	s.statuses = make(map[moduleID]string)
	s.svPublisherStates = make(map[string]*svPublisherManualState)
	s.dashboard = nil
	s.manualPane = nil
	content := s.build()
	restoreAppTabIndex(s.configurationTabs, configurationTabIndex)
	restoreAppTabIndex(s.mainTabs, mainTabIndex)
	s.window.SetContent(content)
}

func selectedAppTabIndex(tabs *container.AppTabs) int {
	if tabs == nil {
		return -1
	}
	return tabs.SelectedIndex()
}

func restoreAppTabIndex(tabs *container.AppTabs, index int) {
	if tabs == nil || index < 0 || index >= len(tabs.Items) {
		return
	}
	tabs.SelectIndex(index)
}

func (s *uiState) disposeModuleViews() {
	if s.manualStop != nil {
		s.manualStop()
		s.manualStop = nil
	}
	s.manualOpenModule = ""
	views := make(map[moduleID]*manualPopOutView, len(s.manualPopOutWindows))
	for id, view := range s.manualPopOutWindows {
		views[id] = view
	}
	for id, view := range views {
		s.closeManualPopOut(id, view)
	}
	if s.manualPopOutWindows == nil {
		s.manualPopOutWindows = make(map[moduleID]*manualPopOutView)
	}
}

func (s *uiState) shutdown() {
	s.stopMCP()
	if s.catalogView != nil {
		s.catalogView.dispose()
		s.catalogView = nil
	}
	if s.sclModelView != nil {
		s.sclModelView.dispose()
	}
	s.disposeModuleViews()
	if s.runtime != nil {
		if err := s.runtime.Close(); err != nil {
			slog.Warn("stop modules during GUI shutdown", "error", err)
		}
		s.runtime = nil
	}
	if s.logLevel != nil {
		if s.configPath != "" {
			slog.Info("project closed", "path", s.configPath)
		}
		if err := s.logLevel.Close(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "PBMT: close project log:", err)
		}
		s.logLevel = nil
	}
}

func (s *uiState) setModuleStatus(id moduleID, status string) {
	s.statuses[id] = status
	s.refreshDashboard()
	s.statusBar.SetText(fmt.Sprintf("%s: %s", moduleTitle(id), status))
}

func (s *uiState) moduleStatus(id moduleID) string {
	if rtID, ok := runtimeModuleID(id); ok && s.runtime != nil {
		status := s.runtime.Status(rtID)
		if status.State == pbmtruntime.StatusError {
			return "Error"
		}
		return string(status.State)
	}
	if status := s.statuses[id]; status != "" {
		return status
	}
	return "Stopped"
}

func (s *uiState) stopModule(id moduleID) error {
	if rtID, ok := runtimeModuleID(id); ok && s.runtime != nil {
		if err := s.runtime.Stop(rtID); err != nil {
			return err
		}
	} else {
		s.statuses[id] = "Stopped"
	}
	s.refreshDashboard()
	s.statusBar.SetText(fmt.Sprintf("%s: Stopped", moduleTitle(id)))
	return nil
}

func (s *uiState) moduleWindowContent(id moduleID, owner fyne.Window) (fyne.CanvasObject, func(), func()) {
	switch id {
	case moduleGoosePub:
		content, cancel := s.goosePublisherControl(owner)
		return content, cancel, nil
	case moduleSVPub:
		return s.svPublisherControl(owner)
	case moduleGooseSub:
		content, cancel := s.gooseSubscriberControl()
		return content, cancel, nil
	case moduleSVSub:
		content, cancel := s.svSubscriberControl()
		return content, cancel, nil
	case modulePTPClient:
		content, cancel := s.ptpClientControl()
		return content, cancel, nil
	case modulePTPServer:
		content, cancel := s.ptpServerControl()
		return content, cancel, nil
	default:
		return widget.NewCard(moduleTitle(id), "", widget.NewLabel("No task view is defined for this module.")), func() {}, nil
	}
}
