package presentation

import (
	"fmt"
	"image/color"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	pbmtruntime "pbmt/internal/application/control"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

func (s *uiState) goosePublisherControl(owner fyne.Window) (fyne.CanvasObject, func()) {
	header := canvas.NewText("GOOSE Publisher Control", color.White)
	header.TextSize = 22
	header.TextStyle = fyne.TextStyle{Bold: true}

	rows := container.NewVBox()
	var cancelAutoSend []func()
	if s.cfg != nil {
		for _, pub := range s.cfg.GoosePub.Publishers {
			pub := pub
			name := pub.Name
			test := widget.NewCheck("Test", nil)
			simulation := widget.NewCheck("Simulation", nil)
			dataControls, dataGrid := newGoosePublisherDatasetGrid(pub.Dataset, owner)
			if s.runtime != nil {
				if snapshot, ok := s.runtime.GoosePublisherSnapshot(name); ok {
					test.SetChecked(snapshot.Test)
					simulation.SetChecked(snapshot.Simulation)
					loadGoosePublisherValues(dataControls, snapshot.Data)
				}
			}
			pending := widget.NewLabel("No pending changes")
			autoSend := widget.NewCheck("Auto Send", nil)
			var applyButton *widget.Button
			var debounceMu sync.Mutex
			var debounceTimer *time.Timer
			disposed := false
			dirty := false
			invalidInputLogged := false

			cancelTimer := func() {
				debounceMu.Lock()
				if debounceTimer != nil {
					debounceTimer.Stop()
					debounceTimer = nil
				}
				debounceMu.Unlock()
			}
			apply := func(interactive bool) {
				cancelTimer()
				if s.runtime == nil {
					err := fmt.Errorf("runtime is not initialized")
					pending.SetText("Pending — " + err.Error())
					if interactive {
						showError(err, owner)
					}
					return
				}
				data, err := goosePublisherDataFromControls(dataControls)
				if err != nil {
					pending.SetText("Pending — invalid value")
					if !invalidInputLogged {
						slog.Warn("GOOSE publisher input rejected", "stream", name, "reason", "invalid_dataset_value")
						invalidInputLogged = true
					}
					if interactive {
						showError(err, owner)
					} else {
						s.statusBar.SetText("GOOSE " + name + ": " + err.Error())
					}
					return
				}
				changed, err := s.runtime.ApplyGoosePublisherState(name, data, test.Checked, simulation.Checked)
				if err != nil {
					pending.SetText("Pending — " + err.Error())
					if interactive {
						showError(err, owner)
					} else {
						s.statusBar.SetText("GOOSE " + name + ": " + err.Error())
					}
					return
				}
				dirty = false
				invalidInputLogged = false
				pending.SetText("No pending changes")
				applyButton.Disable()
				if changed {
					s.statusBar.SetText("GOOSE state change sent: " + name)
				} else {
					s.statusBar.SetText("GOOSE state unchanged: " + name)
				}
			}
			applyButton = widget.NewButtonWithIcon("Apply & Send", theme.UploadIcon(), func() {
				apply(true)
			})
			applyButton.Disable()

			var scheduleAutoSend func()
			scheduleAutoSend = func() {
				debounceMu.Lock()
				if disposed {
					debounceMu.Unlock()
					return
				}
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(400*time.Millisecond, func() {
					fyne.Do(func() {
						debounceMu.Lock()
						stopped := disposed
						debounceMu.Unlock()
						if !stopped && autoSend.Checked && dirty {
							apply(false)
						}
					})
				})
				debounceMu.Unlock()
			}
			markDirty := func() {
				dirty = true
				pending.SetText("Pending changes")
				applyButton.Enable()
				if autoSend.Checked {
					scheduleAutoSend()
				}
			}
			test.OnChanged = func(bool) { markDirty() }
			simulation.OnChanged = func(bool) { markDirty() }
			wireGoosePublisherChangeHandlers(dataControls, markDirty)
			autoSend.OnChanged = func(enabled bool) {
				if enabled && dirty {
					scheduleAutoSend()
				} else if !enabled {
					cancelTimer()
				}
			}
			cancelAutoSend = append(cancelAutoSend, func() {
				debounceMu.Lock()
				disposed = true
				if debounceTimer != nil {
					debounceTimer.Stop()
					debounceTimer = nil
				}
				debounceMu.Unlock()
			})

			meta := fmt.Sprintf("APPID: 0x%04X    Dataset: %s    Entries: %d", uint16(pub.AppID), pub.DatSet, len(pub.Dataset))
			rows.Add(widget.NewCard(pub.Name, meta, container.NewVBox(
				container.NewHBox(test, simulation, autoSend),
				dataGrid,
				container.NewBorder(nil, nil, pending, nil, applyButton),
			)))
		}
	}
	if len(rows.Objects) == 0 {
		rows.Add(widget.NewLabel("No GOOSE publishers configured."))
	}
	return container.NewPadded(container.NewBorder(header, nil, nil, nil, container.NewVScroll(rows))), func() {
		for _, cancel := range cancelAutoSend {
			cancel()
		}
	}
}

type goosePublisherDataControl struct {
	entry        config.GooseDatasetEntry
	boolVal      *widget.Check
	textVal      *widget.Entry
	initialValue *goose.DataValue
	initialText  string
}

// Seed controls before wiring change handlers; opening Manual must not publish.
func loadGoosePublisherValues(controls []goosePublisherDataControl, values []goose.DataValue) {
	if len(controls) != len(values) {
		return
	}
	values = goose.CloneDataValues(values)
	for i := range controls {
		c, value := &controls[i], values[i]
		c.initialValue = &values[i]
		if c.boolVal != nil {
			c.boolVal.SetChecked(value.Bool)
			continue
		}
		text := gooseDataValueText(value)
		switch c.entry.Type {
		case "string":
			text = value.String
		case "utc_time":
			text = ""
			if !value.Time.IsZero() {
				text = value.Time.Format(time.RFC3339Nano)
			}
		case "quality":
			var mask uint32
			for bit := 0; bit < goose.QualityBitLength; bit++ {
				if bit/8 < len(value.Bytes) && value.Bytes[bit/8]&(0x80>>uint(bit%8)) != 0 {
					mask |= 1 << uint(bit)
				}
			}
			text = formatSVPublisherQuality(sv.Quality(mask))
		}
		c.initialText = text
		c.textVal.SetText(text)
	}
}

func newGoosePublisherDatasetGrid(entries []config.GooseDatasetEntry, window fyne.Window) ([]goosePublisherDataControl, fyne.CanvasObject) {
	controls := make([]goosePublisherDataControl, 0, len(entries))
	grid := container.NewVBox(
		container.NewHBox(
			container.NewGridWrap(fyne.NewSize(280, 30), widget.NewLabelWithStyle("Dataset entry", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			container.NewGridWrap(fyne.NewSize(90, 30), widget.NewLabelWithStyle("Type", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			container.NewGridWrap(fyne.NewSize(220, 30), widget.NewLabelWithStyle("Value", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		),
	)
	for _, entry := range entries {
		control := goosePublisherDataControl{entry: entry}
		var value fyne.CanvasObject
		switch entry.Type {
		case "bool":
			check := widget.NewCheck("", nil)
			control.boolVal = check
			value = check
		case "quality":
			quality := widget.NewEntry()
			quality.SetText(formatSVPublisherQuality(sv.QualityValid))
			qualityButton := widget.NewButton("...", func() {
				showSVPublisherQualityDialog(quality, window)
			})
			control.textVal = quality
			value = container.NewBorder(nil, nil, nil, qualityButton, quality)
		default:
			entryValue := widget.NewEntry()
			entryValue.SetPlaceHolder(goosePublisherValuePlaceholder(entry.Type))
			control.textVal = entryValue
			value = entryValue
		}
		controls = append(controls, control)
		grid.Add(container.NewHBox(
			container.NewGridWrap(fyne.NewSize(280, value.MinSize().Height), widget.NewLabel(entry.Name)),
			container.NewGridWrap(fyne.NewSize(90, value.MinSize().Height), widget.NewLabel(entry.Type)),
			container.NewGridWrap(fyne.NewSize(220, value.MinSize().Height), value),
		))
	}
	return controls, grid
}

func wireGoosePublisherChangeHandlers(controls []goosePublisherDataControl, changed func()) {
	for i := range controls {
		if controls[i].boolVal != nil {
			controls[i].boolVal.OnChanged = func(bool) { changed() }
		}
		if controls[i].textVal != nil {
			controls[i].textVal.OnChanged = func(string) { changed() }
		}
	}
}

func goosePublisherValuePlaceholder(t string) string {
	switch t {
	case "int", "uint":
		return "0"
	case "float":
		return "0.0"
	case "utc_time":
		return "now"
	default:
		return ""
	}
}

func goosePublisherDataFromControls(controls []goosePublisherDataControl) ([]goose.DataValue, error) {
	out := make([]goose.DataValue, 0, len(controls))
	for _, control := range controls {
		value, err := goosePublisherControlValue(control)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", control.entry.Name, err)
		}
		out = append(out, value)
	}
	return out, nil
}

func goosePublisherControlValue(control goosePublisherDataControl) (goose.DataValue, error) {
	if control.initialValue != nil {
		unchanged := control.boolVal != nil && control.boolVal.Checked == control.initialValue.Bool
		unchanged = unchanged || (control.textVal != nil && control.textVal.Text == control.initialText)
		if unchanged {
			return goose.CloneDataValues([]goose.DataValue{*control.initialValue})[0], nil
		}
	}
	text := ""
	if control.textVal != nil {
		text = strings.TrimSpace(control.textVal.Text)
	}
	switch control.entry.Type {
	case "bool":
		return goose.DataValue{Type: goose.DataTypeBoolean, Bool: control.boolVal != nil && control.boolVal.Checked}, nil
	case "quality":
		q, err := parseQualityHexEntry(control.textVal)
		if err != nil {
			return goose.DataValue{}, err
		}
		return goose.NewQualityBitString(uint32(q)), nil
	case "int":
		if text == "" {
			text = "0"
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return goose.DataValue{}, err
		}
		return goose.DataValue{Type: goose.DataTypeInteger, Int: value}, nil
	case "uint":
		if text == "" {
			text = "0"
		}
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return goose.DataValue{}, err
		}
		return goose.DataValue{Type: goose.DataTypeUnsigned, UInt: value}, nil
	case "float":
		if text == "" {
			text = "0"
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(text, ",", "."), 32)
		if err != nil {
			return goose.DataValue{}, err
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return goose.DataValue{}, fmt.Errorf("value must be finite")
		}
		return goose.DataValue{Type: goose.DataTypeFloatingPoint, Float: value}, nil
	case "string":
		return goose.DataValue{Type: goose.DataTypeVisibleString, String: text}, nil
	case "utc_time":
		if text != "" {
			value, err := time.Parse(time.RFC3339Nano, text)
			return goose.DataValue{Type: goose.DataTypeUTCTime, Time: value}, err
		}
		return goose.DataValue{Type: goose.DataTypeUTCTime}, nil
	default:
		return goose.DataValue{}, fmt.Errorf("unsupported type %q", control.entry.Type)
	}
}

type gooseDatasetDisplayEntry struct {
	Name    string
	Summary string
	Value   gooseDatasetDisplayNode
}

type gooseDatasetDisplayNode struct {
	Name     string
	Type     string
	Value    string
	Children []gooseDatasetDisplayNode
}

type gooseSubscriberPanel struct {
	description *sclmodel.GooseStream
	modelStatus *widget.Label
	status      *widget.Label
	stream      *widget.Label
	flags       *widget.Label
	dataset     *fyne.Container
	accordion   *widget.Accordion
	empty       *widget.Label
	treeOpen    map[int]map[string]bool
	lastEntries []gooseDatasetDisplayEntry
}

func (s *uiState) gooseSubscriberControl() (fyne.CanvasObject, func()) {
	header := canvas.NewText("GOOSE Monitor", color.White)
	header.TextSize = 22
	header.TextStyle = fyne.TextStyle{Bold: true}
	descriptions, modelMessage := projectGooseDescriptions(s.configPath)
	body := widget.NewLabel(modelMessage)
	body.Wrapping = fyne.TextWrapWord

	panels := make(map[string]*gooseSubscriberPanel)
	streamPanels := container.NewVBox()
	addPanel := func(name string) {
		status := widget.NewLabel("Waiting for GOOSE data...")
		stream := widget.NewLabel("DataSet: -    APPID: -    GoID: -")
		flags := widget.NewLabel("stNum: -    sqNum: -    confRev: -    VLAN: -")
		empty := widget.NewLabel("No decoded Dataset values yet.")
		accordion := widget.NewAccordion()
		accordion.MultiOpen = true
		accordion.Hide()
		dataset := container.NewVBox(empty, accordion)
		panel := &gooseSubscriberPanel{
			modelStatus: widget.NewLabel(""),
			status:      status,
			stream:      stream,
			flags:       flags,
			dataset:     dataset,
			accordion:   accordion,
			empty:       empty,
			treeOpen:    make(map[int]map[string]bool),
		}
		panels[name] = panel
		panel.modelStatus.Wrapping = fyne.TextWrapWord
		streamPanels.Add(widget.NewCard(name, "", container.NewVBox(status, stream, flags, panel.modelStatus, widget.NewSeparator(), dataset)))
	}

	if s.cfg != nil && len(s.cfg.GooseSub.Subscriptions) > 0 {
		for _, sub := range s.cfg.GooseSub.Subscriptions {
			name := strings.TrimSpace(sub.Name)
			if name == "" {
				name = "GOOSE Subscriber"
			}
			addPanel(name)
			panel := panels[name]
			description, message := gooseDescription(sub, descriptions)
			panel.description = description
			panel.modelStatus.SetText(message)
			if description != nil {
				entries, _ := describedGooseEntries(description, nil)
				updateGooseSubscriberDataset(panel, entries)
			}
		}
	} else {
		addPanel("GOOSE Subscriber")
	}

	cancel := func() {}
	if s.runtime != nil {
		// Subscribe before loading snapshots so an opening view cannot miss a
		// transition. Events wake the view; receiver snapshots remain authoritative.
		runtime := s.runtime
		ch, stop := runtime.Subscribe(128)
		refresh := func() {
			for name, panel := range panels {
				snapshots, status := runtime.GooseSubscriberSnapshots(name)
				updateGooseSubscriberSnapshots(panel, snapshots, status)
			}
		}
		refresh()
		done := make(chan struct{})
		var stopped, queued atomic.Bool
		cancel = func() {
			if stopped.CompareAndSwap(false, true) {
				close(done)
				stop()
			}
		}
		queueRefresh := func() {
			if !queued.CompareAndSwap(false, true) {
				return
			}
			fyne.Do(func() {
				queued.Store(false)
				if !stopped.Load() {
					refresh()
				}
			})
		}
		go func() {
			// Heartbeats deliberately generate no events. Reconcile periodically
			// for freshness, lifecycle changes and a dropped best-effort GUI event.
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case event, ok := <-ch:
					if !ok {
						return
					}
					if event.Module == pbmtruntime.ModuleGooseSub {
						queueRefresh()
					}
				case <-ticker.C:
					queueRefresh()
				}
			}
		}()
	}

	content := container.NewVScroll(streamPanels)
	return container.NewPadded(container.NewBorder(container.NewVBox(header, body), nil, nil, nil, content)), cancel
}

func updateGooseSubscriberSnapshots(panel *gooseSubscriberPanel, snapshots []goosesub.Snapshot, module pbmtruntime.ModuleStatus) {
	var latest *goosesub.Snapshot
	for i := range snapshots {
		v := &snapshots[i]
		if v.PDU != nil && (latest == nil || v.ReceivedAt.After(latest.ReceivedAt)) {
			latest = v
		}
	}
	state := string(module.State)
	if module.Error != "" {
		state += ": " + module.Error
	}
	var entries []gooseDatasetDisplayEntry
	if latest == nil {
		setLabelTextIfChanged(panel.status, state+" — waiting for GOOSE data...")
		setLabelTextIfChanged(panel.stream, "DataSet: -    APPID: -    GoID: -")
		setLabelTextIfChanged(panel.flags, "stNum: -    sqNum: -    confRev: -    VLAN: -")
		if panel.description != nil {
			var message string
			entries, message = describedGooseEntries(panel.description, nil)
			setLabelTextIfChanged(panel.modelStatus, message)
		}
	} else {
		pdu := latest.PDU
		entries = gooseDatasetDisplayEntries(pdu.AllData)
		if panel.description != nil {
			var message string
			entries, message = describedGooseEntries(panel.description, pdu)
			setLabelTextIfChanged(panel.modelStatus, message)
		}
		freshness := "stale — last known values"
		if module.State == pbmtruntime.StatusRunning && !latest.Stale {
			freshness = "live"
		}
		status := fmt.Sprintf("%s — %s; last received: %s", state, freshness, latest.ReceivedAt.Format("15:04:05.000"))
		if len(snapshots) > 1 {
			status += fmt.Sprintf("; %d matching streams, showing latest", len(snapshots))
		}
		_, stream, flags := gooseSubscriberEventText(pbmtruntime.Event{}, pdu, len(entries))
		setLabelTextIfChanged(panel.status, status)
		setLabelTextIfChanged(panel.stream, stream)
		setLabelTextIfChanged(panel.flags, flags)
	}
	// Keep existing widgets and expanded branches on unchanged retransmissions.
	if !reflect.DeepEqual(panel.lastEntries, entries) {
		updateGooseSubscriberDataset(panel, entries)
		panel.lastEntries = entries
	}
}

func gooseSubscriberEventText(event pbmtruntime.Event, pdu *goose.PDU, displayedEntries int) (status, stream, flags string) {
	reason := event.Message
	if i := strings.IndexByte(reason, ' '); i >= 0 {
		reason = reason[:i]
	}
	status = fmt.Sprintf("Last event: %s [%s] %s", event.Time.Format("15:04:05.000"), event.Severity, reason)

	entries := fmt.Sprintf("%d", displayedEntries)
	if pdu.NumDatSetEntries != uint32(len(pdu.AllData)) {
		entries = fmt.Sprintf("%d (%d allData decoded / %d declared)", displayedEntries, len(pdu.AllData), pdu.NumDatSetEntries)
	} else if displayedEntries != len(pdu.AllData) {
		entries = fmt.Sprintf("%d (%d allData values)", displayedEntries, len(pdu.AllData))
	}
	stream = fmt.Sprintf("DataSet: %s    APPID: 0x%04X    GoID: %s    Entries: %s", pdu.DatSet, pdu.AppID, pdu.GoID, entries)

	vlan := "untagged"
	if pdu.VLAN != nil {
		vlan = fmt.Sprintf("VID %d / priority %d", pdu.VLAN.VID, pdu.VLAN.Priority)
	}
	flags = fmt.Sprintf(
		"stNum: %d    sqNum: %d    confRev: %d    VLAN: %s    Test: %t    Simulation: %t    ndsCom: %t",
		pdu.StNum,
		pdu.SqNum,
		pdu.ConfRev,
		vlan,
		pdu.Test,
		pdu.Simulation,
		pdu.NdsCom,
	)
	return status, stream, flags
}

func gooseDatasetDisplayEntries(values []goose.DataValue) []gooseDatasetDisplayEntry {
	entries := make([]gooseDatasetDisplayEntry, len(values))
	for i, value := range values {
		entries[i] = gooseDatasetDisplayEntry{
			Name:    fmt.Sprintf("Entry%d", i+1),
			Summary: gooseDataValueText(value),
			Value:   gooseDatasetDisplayValue("Value", value),
		}
	}
	return entries
}

func gooseDatasetDisplayValue(name string, value goose.DataValue) gooseDatasetDisplayNode {
	node := gooseDatasetDisplayNode{
		Name:  name,
		Type:  value.Type.String(),
		Value: gooseDataValueText(value),
	}
	childKind := "Child"
	switch value.Type {
	case goose.DataTypeStructure:
		childKind = "Field"
	case goose.DataTypeArray:
		childKind = "Element"
	}
	node.Children = make([]gooseDatasetDisplayNode, len(value.Children))
	for i, child := range value.Children {
		node.Children[i] = gooseDatasetDisplayValue(fmt.Sprintf("%s %d", childKind, i+1), child)
	}
	return node
}

func gooseDataValueText(value goose.DataValue) string {
	switch value.Type {
	case goose.DataTypeArray, goose.DataTypeStructure:
		return fmt.Sprintf("%d elements", len(value.Children))
	case goose.DataTypeBoolean:
		return strconv.FormatBool(value.Bool)
	case goose.DataTypeInteger, goose.DataTypeBCD:
		return strconv.FormatInt(value.Int, 10)
	case goose.DataTypeUnsigned:
		return strconv.FormatUint(value.UInt, 10)
	case goose.DataTypeFloatingPoint, goose.DataTypeReal:
		return strconv.FormatFloat(value.Float, 'g', -1, 64)
	case goose.DataTypeVisibleString:
		return strconv.Quote(value.String)
	case goose.DataTypeUTCTime:
		if value.Time.IsZero() {
			return "-"
		}
		return value.Time.Format("2006-01-02 15:04:05.000 Z07:00")
	case goose.DataTypeBinaryTime:
		if !value.Time.IsZero() {
			return value.Time.Format("2006-01-02 15:04:05.000 Z07:00")
		}
		return fmt.Sprintf("0x%X", value.Bytes)
	case goose.DataTypeBitString, goose.DataTypeOctetString, goose.DataTypeBooleanArray:
		return fmt.Sprintf("0x%X", value.Bytes)
	default:
		if len(value.Bytes) > 0 {
			return fmt.Sprintf("0x%X", value.Bytes)
		}
		return "-"
	}
}

func updateGooseSubscriberDataset(panel *gooseSubscriberPanel, entries []gooseDatasetDisplayEntry) {
	if panel == nil {
		return
	}
	if len(entries) == 0 {
		panel.accordion.Items = nil
		panel.accordion.Refresh()
		panel.accordion.Hide()
		panel.empty.Show()
		panel.dataset.Refresh()
		return
	}

	open := make([]bool, len(panel.accordion.Items))
	for i, item := range panel.accordion.Items {
		open[i] = item.Open
	}
	items := make([]*widget.AccordionItem, len(entries))
	for i, entry := range entries {
		if panel.treeOpen == nil {
			panel.treeOpen = make(map[int]map[string]bool)
		}
		if panel.treeOpen[i] == nil {
			panel.treeOpen[i] = make(map[string]bool)
		}
		item := widget.NewAccordionItem(
			entry.Name+": "+entry.Summary,
			newGooseDatasetEntryDetails(entry.Value, panel.treeOpen[i]),
		)
		if i < len(open) {
			item.Open = open[i]
		}
		items[i] = item
	}
	panel.accordion.Items = items
	panel.accordion.MultiOpen = true
	panel.empty.Hide()
	panel.accordion.Show()
	panel.accordion.Refresh()
	panel.dataset.Refresh()
}

func newGooseDatasetEntryDetails(value gooseDatasetDisplayNode, open map[string]bool) *fyne.Container {
	header := container.NewGridWithColumns(3,
		widget.NewLabelWithStyle("Field", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabelWithStyle("Type", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabelWithStyle("Value", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	if len(value.Children) == 0 {
		return container.NewVBox(header, newGooseDatasetTreeRow(value))
	}

	nodes, children := indexGooseDatasetTree(value.Children)
	tree := widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID { return children[id] },
		func(id widget.TreeNodeID) bool {
			_, branch := children[id]
			return branch
		},
		func(bool) fyne.CanvasObject { return newGooseDatasetTreeRow(gooseDatasetDisplayNode{}) },
		func(id widget.TreeNodeID, _ bool, object fyne.CanvasObject) {
			setGooseDatasetTreeRow(object.(*fyne.Container), nodes[id])
		},
	)
	tree.HideSeparators = true
	tree.OnBranchOpened = func(id widget.TreeNodeID) { open[id] = true }
	tree.OnBranchClosed = func(id widget.TreeNodeID) { delete(open, id) }
	for id := range open {
		if _, branch := children[id]; !branch {
			delete(open, id)
			continue
		}
		tree.OpenBranch(id)
	}
	minTree := canvas.NewRectangle(color.Transparent)
	height := float32(36 * len(value.Children))
	if height < 96 {
		height = 96
	}
	if height > 300 {
		height = 300
	}
	minTree.SetMinSize(fyne.NewSize(720, height))
	return container.NewVBox(header, newGooseDatasetTreeRow(value), container.NewStack(minTree, tree))
}

func newGooseDatasetTreeRow(value gooseDatasetDisplayNode) *fyne.Container {
	row := container.NewGridWithColumns(3, widget.NewLabel(""), widget.NewLabel(""), widget.NewLabel(""))
	setGooseDatasetTreeRow(row, value)
	return row
}

func setGooseDatasetTreeRow(row *fyne.Container, value gooseDatasetDisplayNode) {
	row.Objects[0].(*widget.Label).SetText(value.Name)
	row.Objects[1].(*widget.Label).SetText(value.Type)
	row.Objects[2].(*widget.Label).SetText(value.Value)
}

func indexGooseDatasetTree(values []gooseDatasetDisplayNode) (map[string]gooseDatasetDisplayNode, map[string][]string) {
	nodes := make(map[string]gooseDatasetDisplayNode)
	children := map[string][]string{"": nil}
	var add func(string, []gooseDatasetDisplayNode)
	add = func(parent string, values []gooseDatasetDisplayNode) {
		ids := make([]string, len(values))
		for i, value := range values {
			id := strconv.Itoa(i + 1)
			if parent != "" {
				id = parent + "." + id
			}
			ids[i] = id
			nodes[id] = value
			if value.Type == goose.DataTypeStructure.String() || value.Type == goose.DataTypeArray.String() {
				add(id, value.Children)
			}
		}
		children[parent] = ids
	}
	add("", values)
	return nodes, children
}
