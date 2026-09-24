package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/catalog"
)

type catalogTreeItem struct {
	name     string
	detail   any
	children []string
}

type mcpCatalogView struct {
	refresh func(bool)
	cancel  context.CancelFunc
}

func (p *mcpCatalogView) dispose() { p.cancel() }

// Viewing and refreshing the catalog never starts a protocol module.
func (s *uiState) mcpCatalogPage() fyne.CanvasObject {
	if s.cfg == nil || s.configPath == "" {
		return container.NewPadded(widget.NewLabel("Open a project to generate its MCP signal catalog."))
	}
	root, cfg := s.configPath, s.cfg
	ctx, cancel := context.WithCancel(context.Background())
	view := &mcpCatalogView{cancel: cancel}
	if s.catalogView != nil {
		s.catalogView.dispose()
	}
	s.catalogView = view
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	note := widget.NewLabel("MCP controls the prepared GOOSE/SV modules; PTP is not exposed. Input/output directions are relative to PBMT. Stop MCP returns control to Manual without changing outputs. Agent connection loss stops both publishers. Modules are never started automatically.")
	note.Wrapping = fyne.TextWrapWord
	detail := widget.NewLabel("Select a module, stream or signal to inspect its description.")
	detail.Wrapping = fyne.TextWrapWord
	items := map[string]catalogTreeItem{"": {}}
	tree := widget.NewTree(func(id string) []string { return items[id].children }, func(id string) bool { return len(items[id].children) > 0 }, func(bool) fyne.CanvasObject { return widget.NewLabel("") }, func(id string, _ bool, obj fyne.CanvasObject) {
		label := obj.(*widget.Label)
		label.Truncation = fyne.TextTruncateEllipsis
		label.SetText(items[id].name)
	})
	tree.OnSelected = func(id string) {
		raw, err := json.MarshalIndent(items[id].detail, "", "  ")
		if err == nil {
			detail.SetText(string(raw))
		}
	}
	lastFingerprint := ""
	lastError := ""
	refresh := func(force bool) {
		if ctx.Err() != nil || s.configPath != root || s.cfg != cfg {
			return
		}
		fingerprint, fingerprintErr := catalog.SourcesFingerprint(root)
		if fingerprintErr != nil {
			fingerprint = "error:" + fingerprintErr.Error()
		}
		if !force && fingerprint == lastFingerprint {
			return
		}
		c, err := catalog.Refresh(root, cfg)
		lastFingerprint = fingerprint
		if c == nil || c.Status == "unavailable" {
			items = map[string]catalogTreeItem{"": {}}
			tree.Refresh()
			detail.SetText("Current catalog is unavailable. Fix the project files and refresh. An unavailable marker replaces the old catalog if the file is writable.")
			message := fmt.Sprint(err)
			status.SetText("Catalog generation failed: " + message)
			if message != lastError {
				slog.Warn("MCP catalog generation failed", "error", err)
			}
			lastError = message
			return
		}
		items = map[string]catalogTreeItem{"": {}}
		add := func(id, parent, name string, v any) {
			items[id] = catalogTreeItem{name: name, detail: v}
			p := items[parent]
			p.children = append(p.children, id)
			items[parent] = p
		}
		var appendSignal func(catalog.Signal, string)
		count := 0
		appendSignal = func(v catalog.Signal, parent string) {
			count++
			add(v.ID, parent, v.Name+" — "+v.Type, v)
			for _, ch := range v.Children {
				appendSignal(ch, v.ID)
			}
		}
		for _, m := range c.Modules {
			enabled := "disabled"
			if m.ConfiguredEnabled {
				enabled = "enabled; runtime not checked"
			}
			add(m.ID, "", m.ID+" — "+m.Direction+" — "+enabled, m)
			for _, st := range m.Streams {
				add(st.ID, m.ID, st.Name+" — "+st.SchemaStatus, st)
				for _, v := range st.Signals {
					appendSignal(v, st.ID)
				}
			}
		}
		tree.CloseAllBranches()
		tree.Refresh()
		for _, m := range c.Modules {
			tree.OpenBranch(m.ID)
		}
		raw, _ := json.MarshalIndent(c.Notes, "", "  ")
		detail.SetText(string(raw))
		if err != nil {
			status.SetText("Catalog generated in memory, but file was NOT updated: " + err.Error())
			if err.Error() != lastError {
				slog.Warn("MCP catalog save failed", "error", err)
			}
			lastError = err.Error()
			return
		}
		lastError = ""
		state := "Saved configuration matches the applied GOOSE/SV settings."
		if c.ConfigurationState == "differs_from_applied" {
			state = "WARNING: saved configuration differs from the applied settings. Running modules were NOT reconfigured."
		}
		status.SetText(fmt.Sprintf("Project: %s\n%s\n%d signal/schema nodes; %s. Updated %s.\n%s\nProject files are checked every 2 seconds.", c.Project, filepath.Join(root, catalog.FileName), count, c.Status, c.GeneratedAt.Local().Format("2006-01-02 15:04:05"), state))
		slog.Info("MCP catalog generated", "path", filepath.Join(root, catalog.FileName), "revision", c.Revision, "signal_nodes", count)
	}
	view.refresh = refresh
	actions := newMCPCatalogActions(func() { refresh(true) })
	serverPanel, updateServer := s.mcpServerPanel(actions)
	split := container.NewHSplit(tree, container.NewVScroll(detail))
	split.Offset = 0.48
	refresh(true)
	// Coalesce notifications if the UI is busy. Cancellation also guards any
	// already queued update after project switching or application shutdown.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var queued atomic.Bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if queued.CompareAndSwap(false, true) {
					fyne.Do(func() {
						defer queued.Store(false)
						if ctx.Err() == nil {
							updateServer()
							refresh(false)
						}
					})
				}
			}
		}
	}()
	header := container.NewGridWithColumns(2, container.NewVBox(note, status), serverPanel)
	return container.NewPadded(container.NewBorder(container.NewVBox(header, actions), nil, nil, nil, split))
}

func newMCPCatalogActions(refresh func()) *fyne.Container {
	refreshButton := widget.NewButton("Refresh Catalog", refresh)
	startButton := widget.NewButton("Start MCP", nil)
	stopButton := widget.NewButton("Stop MCP", nil)
	// Enabled by mcpServerPanel only when the runtime supports exclusive control.
	startButton.Disable()
	stopButton.Disable()
	return container.NewGridWithColumns(3, refreshButton, startButton, stopButton)
}
