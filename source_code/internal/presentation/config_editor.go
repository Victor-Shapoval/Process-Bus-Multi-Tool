package presentation

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"gopkg.in/yaml.v3"

	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/scl"
	"pbmt/internal/domain/sv"
)

func (s *uiState) configEditorPage(id moduleID) fyne.CanvasObject {
	if s.configPath == "" {
		msg := widget.NewLabel("Create or open a project first.")
		msg.Alignment = fyne.TextAlignCenter
		return container.NewPadded(msg)
	}
	path := filepath.Join(s.projectConfigDir(), moduleConfigFile(id))
	root, err := readYAMLRoot(path)
	if err != nil {
		slog.Error("configuration read failed", "module", id, "path", path, "error", err)
		s.statusBar.SetText(fmt.Sprintf("Read %s: %v", path, err))
		msg := widget.NewLabel(err.Error())
		msg.Wrapping = fyne.TextWrapWord
		return container.NewPadded(msg)
	}
	removeLegacyPTPTimestampModeConfig(root, id)
	ensurePTPSourceConfig(root, id)
	ensurePTPDelayMechanismConfig(root, id)
	ensurePTPPowerProfileConfig(root, id)
	ensurePTPTraceabilityConfig(root, id)
	body := container.NewVBox()
	var render func()
	render = func() {
		body.Objects = []fyne.CanvasObject{s.yamlNodeEditor(root, render)}
		body.Refresh()
	}
	render()

	save := widget.NewButtonWithIcon("Save", theme.DocumentSaveIcon(), func() {
		if s.mcpActive() {
			showError(fmt.Errorf("stop MCP before saving configuration"), s.window)
			return
		}
		doc := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
		data, err := yaml.Marshal(&doc)
		if err != nil {
			showError(err, s.window)
			return
		}
		cfg, err := config.SaveProfileFile(s.configPath, moduleConfigFile(id), data)
		if err != nil {
			showError(err, s.window)
			s.statusBar.SetText("Configuration was not saved")
			return
		}
		slog.Info("configuration saved", "module", id, "path", path)
		s.activateProjectConfig(cfg)
		s.statusBar.SetText("Saved: " + moduleTitle(id))
	})

	actions := []fyne.CanvasObject{layout.NewSpacer()}
	if addKey := addableSequenceKey(id); addKey != "" {
		add := widget.NewButtonWithIcon("Add", theme.ContentAddIcon(), func() {
			if err := addConfigItem(root, id, addKey, s.deps.LoadConfigItemTemplate); err != nil {
				showError(err, s.window)
				return
			}
			render()
			s.statusBar.SetText("Added item to " + humanConfigLabel(addKey))
		})
		actions = append(actions, add)
	}
	actions = append(actions, save)

	return container.NewBorder(nil, container.NewPadded(container.NewHBox(actions...)), nil, nil, container.NewVScroll(body))
}

// removeLegacyPTPTimestampModeConfig migrates projects created by releases
// that exposed timestamp selection. Client policy and the hardware-only Server
// policy are now application invariants rather than project settings.
func removeLegacyPTPTimestampModeConfig(root *yaml.Node, id moduleID) {
	if id != modulePTPClient && id != modulePTPServer {
		return
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); {
		if root.Content[i].Value == "timestamp_mode" {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			continue
		}
		i += 2
	}
}

func ensurePTPSourceConfig(root *yaml.Node, id moduleID) {
	if id != modulePTPClient || root == nil || root.Kind != yaml.MappingNode || mappingValue(root, "source") != nil {
		return
	}
	root.Content = append([]*yaml.Node{scalarString("source"), scalarString("external")}, root.Content...)
}

// ensurePTPDelayMechanismConfig makes the independent mechanism selector
// available when an older project without delay_mechanism is opened.
func ensurePTPDelayMechanismConfig(root *yaml.Node, id moduleID) {
	if id != modulePTPClient && id != modulePTPServer {
		return
	}
	if root == nil || root.Kind != yaml.MappingNode || mappingValue(root, "delay_mechanism") != nil {
		return
	}

	mechanism := "e2e"
	if profile := mappingValue(root, "profile"); profile != nil && profile.Value == "power" {
		mechanism = "p2p"
	}
	pair := []*yaml.Node{scalarString("delay_mechanism"), scalarString(mechanism)}
	insertAt := len(root.Content)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "profile" {
			insertAt = i + 2
			break
		}
	}
	root.Content = append(root.Content[:insertAt], append(pair, root.Content[insertAt:]...)...)
}

// ensurePTPPowerProfileConfig exposes the expected C37.238 version on a
// client and the transmitted TLV fields on a server in older projects.
func ensurePTPPowerProfileConfig(root *yaml.Node, id moduleID) {
	if (id != modulePTPClient && id != modulePTPServer) || root == nil || root.Kind != yaml.MappingNode || mappingValue(root, "power_profile") != nil {
		return
	}
	if id == modulePTPClient {
		root.Content = append(root.Content,
			scalarString("power_profile"),
			mappingNode("version", scalarString("2011")),
		)
		return
	}
	power := mappingNode(
		"version", scalarString("2011"),
		"grandmaster_id", scalarInt("3"),
		"grandmaster_time_inaccuracy", scalarInt("60"),
		"network_time_inaccuracy", scalarInt("0"),
		"total_time_inaccuracy", scalarInt("100"),
		"alternate_time_offset", mappingNode(
			"enabled", scalarBool(true),
			"key_field", scalarInt("1"),
			"current_offset", scalarInt("10763"),
			"jump_seconds", scalarInt("0"),
			"time_of_next_jump", scalarInt("0"),
			"display_name", scalarString("UTC+03:00"),
		),
	)
	root.Content = append(root.Content, scalarString("power_profile"), power)
}

// ensurePTPTraceabilityConfig exposes explicit, conservative traceability
// switches when an older PTP Server project did not contain them.
func ensurePTPTraceabilityConfig(root *yaml.Node, id moduleID) {
	if id != modulePTPServer || root == nil || root.Kind != yaml.MappingNode {
		return
	}
	for _, key := range []string{"time_traceable", "frequency_traceable"} {
		if mappingValue(root, key) == nil {
			root.Content = append(root.Content, scalarString(key), scalarBool(false))
		}
	}
}

func readYAMLRoot(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	return doc.Content[0], nil
}

func (s *uiState) yamlNodeEditor(node *yaml.Node, refresh func()) fyne.CanvasObject {
	switch node.Kind {
	case yaml.MappingNode:
		return s.yamlMappingEditor(node, refresh)
	case yaml.SequenceNode:
		return s.yamlSequenceEditor("", node, refresh)
	case yaml.ScalarNode:
		return scalarWidget("", node)
	default:
		return widget.NewLabel("Unsupported configuration node")
	}
}

func (s *uiState) yamlMappingEditor(node *yaml.Node, refresh func()) fyne.CanvasObject {
	form := widget.NewForm()
	sections := container.NewVBox()
	powerProfile := powerProfileSelected(node)
	if powerProfile {
		if mechanism := mappingValue(node, "delay_mechanism"); mechanism != nil {
			setScalarValue("delay_mechanism", mechanism, "p2p")
		}
		if transport := mappingValue(node, "transport"); transport != nil {
			setScalarValue("transport", transport, "ethernet")
		}
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		if source := mappingValue(node, "source"); source != nil && source.Value == "local" {
			switch key {
			case "interface", "profile", "transport", "domain_number", "delay_mechanism", "power_profile":
				continue
			}
		}
		if key == "signals" || key == "data" {
			continue
		}
		if !configMappingVisible(node, key) {
			continue
		}

		switch value.Kind {
		case yaml.ScalarNode:
			if key == "transport" && powerProfile {
				form.Append(humanConfigLabel(key), widget.NewLabel("Ethernet L2 (required by Power Profile)"))
				continue
			}
			if key == "delay_mechanism" && powerProfile {
				form.Append(humanConfigLabel(key), widget.NewLabel("P2P (required by Power Profile)"))
				continue
			}
			if key == "smp_rate" && isSVPublisherMapping(node) {
				form.Append(humanConfigLabel(key), configSelect(key, value, []string{"80"}, nil))
				continue
			}
			var onSelect func()
			if key == "profile" || key == "source" {
				onSelect = refresh
			}
			form.Append(humanConfigLabel(key), scalarWidget(key, value, onSelect))
		case yaml.SequenceNode:
			if key == "dataset" {
				form.Append(humanConfigLabel(key), container.NewHBox(
					widget.NewButtonWithIcon("Dataset", theme.ListIcon(), func() {
						s.showGooseDatasetWindow(node, refresh)
					}),
					widget.NewButtonWithIcon("Export", theme.DocumentSaveIcon(), func() {
						s.showGooseExportDialog(node)
					}),
				))
				continue
			}
			if sequenceHasOnlyScalars(value) {
				form.Append(humanConfigLabel(key), scalarSequenceEntry(value))
				continue
			}
			sections.Add(s.yamlSequenceEditor(key, value, refresh))
		case yaml.MappingNode:
			sections.Add(widget.NewCard(humanConfigLabel(key), "", s.yamlMappingEditor(value, refresh)))
		}
	}
	if isSVPublisherMapping(node) {
		form.Append("SCL model", widget.NewButtonWithIcon("Export", theme.DocumentSaveIcon(), func() {
			s.showSVExportDialog(node)
		}))
	}

	items := container.NewVBox()
	if len(form.Items) > 0 {
		items.Add(form)
	}
	for _, section := range sections.Objects {
		items.Add(section)
	}
	return items
}

func configMappingVisible(parent *yaml.Node, key string) bool {
	if key != "power_profile" {
		return true
	}
	return powerProfileSelected(parent)
}

func powerProfileSelected(parent *yaml.Node) bool {
	profile := mappingValue(parent, "profile")
	return profile != nil && profile.Value == "power"
}

func (s *uiState) yamlSequenceEditor(key string, node *yaml.Node, refresh func()) fyne.CanvasObject {
	items := container.NewVBox()
	for i, item := range node.Content {
		title := strconv.Itoa(i + 1)
		header := container.NewHBox(widget.NewLabel(title), layout.NewSpacer())
		if key == "subscriptions" && mappingValue(item, "gocb_ref") != nil {
			header.Add(widget.NewButton("Import SCL", func() { s.showGooseImportDialog(item, refresh) }))
		}
		if i > 0 {
			idx := i
			header.Add(widget.NewButtonWithIcon("Delete", theme.DeleteIcon(), func() {
				node.Content = append(node.Content[:idx], node.Content[idx+1:]...)
				if refresh != nil {
					refresh()
				}
			}))
		}
		items.Add(widget.NewCard("", "", container.NewBorder(header, nil, nil, nil, s.yamlNodeEditor(item, refresh))))
	}
	if len(node.Content) == 0 {
		items.Add(widget.NewLabel("No items configured."))
	}
	return widget.NewCard(humanConfigLabel(key), "", items)
}

func scalarWidget(key string, node *yaml.Node, onSelect ...func()) fyne.CanvasObject {
	if node.Tag == "!!bool" || node.Value == "true" || node.Value == "false" {
		check := widget.NewCheck("", func(v bool) {
			node.Kind = yaml.ScalarNode
			node.Tag = "!!bool"
			node.Value = strconv.FormatBool(v)
		})
		check.SetChecked(node.Value == "true")
		return check
	}
	if key == "interface" {
		return networkInterfaceSelect(node)
	}
	if options := configSelectOptions(key); len(options) > 0 {
		var changed func()
		if len(onSelect) > 0 {
			changed = onSelect[0]
		}
		return configSelect(key, node, options, changed)
	}

	entry := widget.NewEntry()
	entry.Wrapping = fyne.TextWrapOff
	entry.Scroll = container.ScrollNone
	entry.SetText(node.Value)
	entry.OnChanged = func(v string) {
		setScalarValue(key, node, v)
	}
	return entry
}

func configSelect(key string, node *yaml.Node, options []string, changed func()) fyne.CanvasObject {
	if key == "source" {
		selectBox := widget.NewSelect([]string{"Local", "External"}, nil)
		selectBox.SetSelected(map[string]string{"local": "Local", "external": "External"}[node.Value])
		selectBox.OnChanged = func(value string) {
			setScalarValue(key, node, strings.ToLower(value))
			if changed != nil {
				changed()
			}
		}
		return selectBox
	}
	selected := node.Value
	if selected != "" && !stringInSlice(options, selected) {
		options = append([]string{selected}, options...)
	}
	initializing := true
	selectBox := widget.NewSelect(options, func(v string) {
		previous := node.Value
		setScalarValue(key, node, v)
		if !initializing && previous != v && changed != nil {
			changed()
		}
	})
	selectBox.PlaceHolder = "Select " + strings.ToLower(humanConfigLabel(key))
	selectBox.SetSelected(selected)
	initializing = false
	return selectBox
}
func configSelectOptions(key string) []string {
	switch key {
	case "source":
		return []string{"local", "external"}
	case "profile":
		return []string{"default", "power"}
	case "transport":
		return []string{"udp", "ethernet"}
	case "delay_mechanism":
		return []string{"e2e", "p2p"}
	case "version":
		return []string{"2011", "2017"}
	case "time_source":
		return []string{"gps", "ptp", "ntp", "internal_oscillator", "atomic_clock", "terrestrial_radio", "serial_time_code", "hand_set", "other"}
	case "smp_rate":
		return []string{"80", "256"}
	case "sample_timing_frequency":
		return []string{"50", "60"}
	case "smp_synch":
		return []string{"0", "1", "2"}
	case "type":
		return []string{"bool", "quality", "int", "uint", "float", "string", "utc_time"}
	case "base_vector":
		return []string{"Ua", "Ub", "Uc", "Un", "Ia", "Ib", "Ic", "In"}
	default:
		return nil
	}
}

func networkInterfaceSelect(node *yaml.Node) fyne.CanvasObject {
	names := networkInterfaceNames()
	if node.Value != "" && !stringInSlice(names, node.Value) {
		names = append([]string{node.Value}, names...)
	}
	selectBox := widget.NewSelect(names, func(v string) {
		setScalarValue("interface", node, v)
	})
	selectBox.PlaceHolder = "Select network interface"
	selectBox.SetSelected(node.Value)
	return selectBox
}

func scalarSequenceEntry(node *yaml.Node) fyne.CanvasObject {
	entry := widget.NewEntry()
	entry.Wrapping = fyne.TextWrapOff
	entry.Scroll = container.ScrollNone
	entry.SetText(joinScalarSequence(node))
	entry.OnChanged = func(v string) {
		node.Kind = yaml.SequenceNode
		node.Content = node.Content[:0]
		for _, raw := range strings.Split(v, ",") {
			part := strings.TrimSpace(raw)
			if part == "" {
				continue
			}
			child := &yaml.Node{Kind: yaml.ScalarNode}
			setScalarValue("", child, part)
			node.Content = append(node.Content, child)
		}
	}
	return entry
}

func setScalarValue(key string, node *yaml.Node, value string) {
	node.Kind = yaml.ScalarNode
	if value == "" && optionalConfigField(key) {
		node.Tag = "!!null"
		node.Value = ""
		return
	}
	node.Value = value
	switch {
	case strings.EqualFold(value, "true") || strings.EqualFold(value, "false"):
		node.Tag = "!!bool"
	case looksInteger(value):
		node.Tag = "!!int"
	default:
		node.Tag = "!!str"
	}
}

func sequenceHasOnlyScalars(node *yaml.Node) bool {
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			return false
		}
	}
	return true
}

func joinScalarSequence(node *yaml.Node) string {
	parts := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		parts = append(parts, item.Value)
	}
	return strings.Join(parts, ", ")
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		node.Kind = yaml.MappingNode
		node.Content = nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, scalarString(key), value)
}

func ensureMappingSequence(node *yaml.Node, key string) *yaml.Node {
	if value := mappingValue(node, key); value != nil && value.Kind == yaml.SequenceNode {
		return value
	}
	seq := sequenceNode()
	setMappingValue(node, key, seq)
	return seq
}

func (s *uiState) showGooseDatasetWindow(publisher *yaml.Node, refreshConfig func()) {
	dataset := ensureMappingSequence(publisher, "dataset")

	w := s.app.NewWindow("GOOSE Dataset")
	w.SetIcon(pbmtIcon)
	status := widget.NewLabel("")
	rows := container.NewVBox()

	var renderRows func()
	renderRows = func() {
		rows.Objects = nil
		if len(dataset.Content) == 0 {
			rows.Add(widget.NewLabel("Dataset is empty."))
			rows.Refresh()
			return
		}
		rows.Add(container.NewHBox(
			container.NewGridWrap(fyne.NewSize(52, 30), widget.NewLabelWithStyle("#", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			container.NewGridWrap(fyne.NewSize(360, 30), widget.NewLabelWithStyle("Name", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			container.NewGridWrap(fyne.NewSize(140, 30), widget.NewLabelWithStyle("Type", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		))
		for i, item := range dataset.Content {
			if item.Kind != yaml.MappingNode {
				item.Kind = yaml.MappingNode
				item.Content = nil
			}
			nameNode := mappingValue(item, "name")
			if nameNode == nil {
				nameNode = scalarString(fmt.Sprintf("Signal%d", i+1))
				setMappingValue(item, "name", nameNode)
			}
			typeNode := mappingValue(item, "type")
			if typeNode == nil {
				typeNode = scalarString("bool")
				setMappingValue(item, "type", typeNode)
			}
			nameEntry := widget.NewEntry()
			nameEntry.SetText(nameNode.Value)
			nameEntry.OnChanged = func(v string) {
				setScalarValue("name", nameNode, v)
			}
			typeSelect := widget.NewSelect(configSelectOptions("type"), func(v string) {
				setScalarValue("type", typeNode, v)
			})
			typeSelect.SetSelected(typeNode.Value)
			rows.Add(container.NewHBox(
				container.NewGridWrap(fyne.NewSize(52, nameEntry.MinSize().Height), widget.NewLabel(strconv.Itoa(i+1))),
				container.NewGridWrap(fyne.NewSize(360, nameEntry.MinSize().Height), nameEntry),
				container.NewGridWrap(fyne.NewSize(140, nameEntry.MinSize().Height), typeSelect),
			))
		}
		rows.Refresh()
	}

	header := container.NewVBox(
		widget.NewLabelWithStyle("GOOSE Dataset", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Dataset is saved into goose_pub.yaml when you press Save in Configuration."),
	)

	regenerate := widget.NewButtonWithIcon("Regenerate 32 bool + quality", theme.ViewRefreshIcon(), func() {
		dataset = gooseFixedBoolQualityDatasetNode()
		setMappingValue(publisher, "dataset", dataset)
		status.SetText("Generated 64 dataset entries.")
		renderRows()
		if refreshConfig != nil {
			refreshConfig()
		}
	})

	closeButton := widget.NewButton("OK", func() {
		if refreshConfig != nil {
			refreshConfig()
		}
		w.Close()
	})
	renderRows()
	w.SetContent(container.NewBorder(
		container.NewPadded(header),
		container.NewPadded(container.NewHBox(status, layout.NewSpacer(), regenerate, closeButton)),
		nil,
		nil,
		container.NewVScroll(rows),
	))
	w.Resize(fyne.NewSize(680, 620))
	w.Show()
}

func (s *uiState) showGooseExportDialog(publisher *yaml.Node) {
	data, err := yaml.Marshal(publisher)
	if err != nil {
		showError(fmt.Errorf("encode publisher configuration: %w", err), s.window)
		return
	}
	var cfg config.GoosePublisher
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		showError(fmt.Errorf("read publisher configuration: %w", err), s.window)
		return
	}

	dataset := make([]scl.DatasetEntry, len(cfg.Dataset))
	for i, entry := range cfg.Dataset {
		dataset[i] = scl.DatasetEntry{Name: entry.Name, Type: entry.Type}
	}
	sclData, err := scl.MarshalGoosePublisher(scl.GoosePublisher{
		Name:         cfg.Name,
		DstMAC:       cfg.DstMAC,
		AppID:        uint16(cfg.AppID),
		GocbRef:      cfg.GocbRef,
		DatSet:       cfg.DatSet,
		GoID:         cfg.GoID,
		ConfRev:      cfg.ConfRev,
		VLANID:       cfg.VLANID,
		VLANPriority: cfg.VLANPri,
		MinTimeMS:    cfg.MinIntervalMs,
		MaxTimeMS:    cfg.MaxIntervalMs,
		Dataset:      dataset,
	})
	if err != nil {
		showError(fmt.Errorf("export SCL: %w", err), s.window)
		return
	}
	s.showSCLSaveDialog(sclData, gooseSCLFileName(cfg.Name), "GOOSE")
}

func (s *uiState) showSVExportDialog(stream *yaml.Node) {
	data, err := yaml.Marshal(stream)
	if err != nil {
		showError(fmt.Errorf("encode SV publisher configuration: %w", err), s.window)
		return
	}
	var cfg config.SVPublisher
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		showError(fmt.Errorf("read SV publisher configuration: %w", err), s.window)
		return
	}

	sclData, err := scl.MarshalSVPublisher(scl.SVPublisher{
		Name:                  cfg.Name,
		DstMAC:                cfg.DstMAC,
		AppID:                 uint16(cfg.AppID),
		SvID:                  cfg.SvID,
		DatSet:                cfg.DatSet,
		ConfRev:               cfg.ConfRev,
		VLANID:                cfg.VLANID,
		VLANPriority:          cfg.VLANPri,
		SmpRate:               cfg.SmpRate,
		SampleTimingFrequency: cfg.SampleTimingFrequency,
	})
	if err != nil {
		showError(fmt.Errorf("export SV SCL: %w", err), s.window)
		return
	}
	s.showSCLSaveDialog(sclData, svSCLFileName(cfg.Name), "SV")
}

func (s *uiState) showSCLSaveDialog(sclData []byte, fileName, kind string) {
	save := dialog.NewFileSave(func(writer fyne.URIWriteCloser, err error) {
		if err != nil {
			showError(err, s.window)
			return
		}
		if writer == nil {
			return
		}
		exportedName := writer.URI().Name()
		_, writeErr := writer.Write(sclData)
		closeErr := writer.Close()
		if writeErr != nil {
			showError(fmt.Errorf("write %s SCL: %w", kind, writeErr), s.window)
			return
		}
		if closeErr != nil {
			showError(fmt.Errorf("close %s SCL: %w", kind, closeErr), s.window)
			return
		}
		s.statusBar.SetText(fmt.Sprintf("Exported %s SCL: %s", kind, exportedName))
		slog.Info("publisher SCL exported", "protocol", kind, "path", writer.URI().Path())
	}, s.window)
	save.SetFilter(storage.NewExtensionFileFilter([]string{".cid", ".icd", ".scd"}))
	save.SetFileName(fileName)
	save.Show()
}

func isSVPublisherMapping(node *yaml.Node) bool {
	return mappingValue(node, "sv_id") != nil &&
		mappingValue(node, "conf_rev") != nil &&
		mappingValue(node, "dst_mac") != nil &&
		mappingValue(node, "smp_rate") != nil &&
		mappingValue(node, "match_any_app_id") == nil
}

func gooseSCLFileName(name string) string {
	return sclFileName(name, "pbmt_goose")
}

func svSCLFileName(name string) string {
	return sclFileName(name, "pbmt_sv")
}

func sclFileName(name, fallback string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = fallback
	}
	return base + ".icd"
}

func addableSequenceKey(id moduleID) string {
	switch id {
	case moduleGooseSub, moduleSVSub:
		return "subscriptions"
	case moduleGoosePub:
		return "publishers"
	case moduleSVPub:
		return "streams"
	default:
		return ""
	}
}

func addConfigItem(root *yaml.Node, id moduleID, key string, loadTemplate func(string) ([]byte, error)) error {
	seq := mappingSequence(root, key)
	if seq == nil {
		return fmt.Errorf("configuration section %q not found", key)
	}
	if loadTemplate == nil {
		return fmt.Errorf("configuration item template loader is not configured")
	}
	data, err := loadTemplate(moduleConfigFile(id))
	if err != nil {
		return fmt.Errorf("load %s item template: %w", moduleConfigFile(id), err)
	}
	item, err := parseConfigItemTemplate(data)
	if err != nil {
		return fmt.Errorf("parse %s item template: %w", moduleConfigFile(id), err)
	}
	if err := makeConfigItemNameUnique(item, seq); err != nil {
		return fmt.Errorf("prepare %s item template: %w", moduleConfigFile(id), err)
	}
	if err := makeConfigItemDstMACUnique(item, seq, id); err != nil {
		return fmt.Errorf("prepare %s item template: %w", moduleConfigFile(id), err)
	}
	seq.Content = append(seq.Content, item)
	return nil
}

func mappingSequence(root *yaml.Node, key string) *yaml.Node {
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key && root.Content[i+1].Kind == yaml.SequenceNode {
			return root.Content[i+1]
		}
	}
	return nil
}

func parseConfigItemTemplate(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("template must contain one mapping")
	}
	return doc.Content[0], nil
}

func makeConfigItemNameUnique(item, seq *yaml.Node) error {
	name := mappingValue(item, "name")
	if name == nil || name.Kind != yaml.ScalarNode || strings.TrimSpace(name.Value) == "" {
		return fmt.Errorf("template name is required")
	}

	used := make(map[string]struct{}, len(seq.Content))
	for _, existing := range seq.Content {
		if existingName := mappingValue(existing, "name"); existingName != nil {
			used[existingName.Value] = struct{}{}
		}
	}
	base := name.Value
	if _, exists := used[base]; !exists {
		return nil
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s_%d", base, suffix)
		if _, exists := used[candidate]; exists {
			continue
		}
		name.Tag = "!!str"
		name.Value = candidate
		return nil
	}
}

func makeConfigItemDstMACUnique(item, seq *yaml.Node, id moduleID) error {
	dst := mappingValue(item, "dst_mac")
	if dst == nil || dst.Kind != yaml.ScalarNode || strings.TrimSpace(dst.Value) == "" {
		return fmt.Errorf("template dst_mac is required")
	}
	base, err := net.ParseMAC(dst.Value)
	if err != nil || len(base) != 6 {
		return fmt.Errorf("template dst_mac %q is not a 48-bit MAC address", dst.Value)
	}
	if !isModuleMulticastMAC(id, base) {
		return fmt.Errorf("template dst_mac %q is outside the module multicast range", dst.Value)
	}

	used := make(map[string]struct{}, len(seq.Content))
	for _, existing := range seq.Content {
		existingDst := mappingValue(existing, "dst_mac")
		if existingDst == nil {
			continue
		}
		mac, parseErr := net.ParseMAC(existingDst.Value)
		if parseErr == nil && len(mac) == 6 {
			used[mac.String()] = struct{}{}
		}
	}

	const multicastAddressCount = 0x0200
	baseLow := uint16(base[4])<<8 | uint16(base[5])
	for offset := uint16(0); offset < multicastAddressCount; offset++ {
		low := (baseLow + offset) % multicastAddressCount
		candidate := append(net.HardwareAddr(nil), base...)
		candidate[4] = byte(low >> 8)
		candidate[5] = byte(low)
		if _, exists := used[candidate.String()]; exists {
			continue
		}
		dst.Tag = "!!str"
		dst.Value = candidate.String()
		return nil
	}
	return fmt.Errorf("all multicast MAC addresses are already in use")
}

func isModuleMulticastMAC(id moduleID, mac net.HardwareAddr) bool {
	switch id {
	case moduleGoosePub, moduleGooseSub:
		return goose.IsValidMulticastMAC(mac)
	case moduleSVPub, moduleSVSub:
		return sv.IsValidMulticastMAC(mac)
	default:
		return false
	}
}

func mappingNode(pairs ...interface{}) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		value, _ := pairs[i+1].(*yaml.Node)
		node.Content = append(node.Content, scalarString(key), value)
	}
	return node
}

func sequenceNode(items ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Content: items}
}

func gooseFixedBoolQualityDatasetNode() *yaml.Node {
	items := make([]*yaml.Node, 0, 64)
	for i := 1; i <= 32; i++ {
		items = append(items,
			mappingNode(
				"name", scalarString(fmt.Sprintf("Signal%d.stVal", i)),
				"type", scalarString("bool"),
			),
			mappingNode(
				"name", scalarString(fmt.Sprintf("Signal%d.q", i)),
				"type", scalarString("quality"),
			),
		)
	}
	return sequenceNode(items...)
}

func scalarString(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func scalarInt(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value}
}

func scalarBool(value bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(value)}
}

func humanConfigLabel(key string) string {
	labels := map[string]string{
		"enabled":                     "Enable module",
		"interface":                   "Network interface",
		"promiscuous":                 "Promiscuous mode",
		"subscriptions":               "Subscriptions",
		"publishers":                  "Publishers",
		"streams":                     "Publishers",
		"name":                        "Name",
		"dst_mac":                     "Destination MAC",
		"src_mac":                     "Source MAC",
		"app_id":                      "APP ID",
		"match_any_app_id":            "Accept any APP ID",
		"allow_nonstandard":           "Allow non-standard protocol values",
		"gocb_ref":                    "GoCB reference",
		"go_id":                       "GO ID",
		"dat_set":                     "Dataset",
		"sv_id":                       "SV ID",
		"conf_rev":                    "Configuration revision",
		"vlan_id":                     "VLAN ID",
		"vlan_pri":                    "VLAN priority",
		"nds_com":                     "Needs commissioning",
		"accept_test":                 "Accept test messages",
		"accept_simulation":           "Accept simulation messages",
		"accept_nds_com":              "Accept uncommissioned messages",
		"min_interval_ms":             "Minimum interval, ms",
		"max_interval_ms":             "Maximum interval, ms",
		"dataset":                     "Dataset",
		"type":                        "Type",
		"value":                       "Value",
		"children":                    "Children",
		"path":                        "Signal path",
		"profile":                     "PTP profile",
		"source":                      "Source PTP",
		"transport":                   "Transport",
		"domain_number":               "Domain number",
		"delay_mechanism":             "Delay mechanism",
		"utc_offset":                  "UTC offset",
		"time_source":                 "Time source",
		"priority1":                   "Priority 1",
		"priority2":                   "Priority 2",
		"clock_class":                 "Clock class",
		"clock_accuracy":              "Clock accuracy",
		"offset_scaled_log_variance":  "Offset scaled log variance",
		"time_traceable":              "Time traceable",
		"frequency_traceable":         "Frequency traceable",
		"power_profile":               "Power profile (IEEE C37.238)",
		"version":                     "C37.238 version",
		"grandmaster_id":              "Grandmaster ID",
		"grandmaster_time_inaccuracy": "Grandmaster time inaccuracy, ns",
		"network_time_inaccuracy":     "Network time inaccuracy, ns",
		"total_time_inaccuracy":       "Total time inaccuracy, ns",
		"alternate_time_offset":       "Alternate time offset",
		"key_field":                   "Key field",
		"current_offset":              "Current offset, s",
		"jump_seconds":                "Jump seconds",
		"time_of_next_jump":           "Time of next jump",
		"display_name":                "Display name",
		"smp_rate":                    "Samples per period",
		"sample_timing_frequency":     "Sample timing frequency",
		"smp_synch":                   "Sample sync",
		"base_vector":                 "Base vector",
		"current_coefficient":         "Current coefficient KI",
		"voltage_coefficient":         "Voltage coefficient KU",
	}
	if label, ok := labels[key]; ok {
		return label
	}
	key = strings.ReplaceAll(key, "_", " ")
	if key == "" {
		return "Configuration"
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

func optionalConfigField(key string) bool {
	switch key {
	case "vlan_id", "vlan_pri", "conf_rev", "domain_number", "utc_offset", "priority1", "priority2", "clock_class", "clock_accuracy", "offset_scaled_log_variance":
		return true
	default:
		return false
	}
}

func looksInteger(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X") {
		_, err := strconv.ParseUint(value[2:], 16, 64)
		return err == nil
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}
