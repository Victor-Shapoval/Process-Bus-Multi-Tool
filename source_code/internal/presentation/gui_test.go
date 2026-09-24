package presentation

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"gopkg.in/yaml.v3"

	pbmtruntime "pbmt/internal/application/control"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
	"pbmt/profiles"
)

func TestProjectBrowserShowsDirectoriesYAMLAndLogFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zeta", "cfg"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"goose_pub.yaml", "PROJECT.LOG", "README.md", "backup.yaml.bak"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	directoryEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := projectBrowserEntries(directoryEntries)
	want := []projectBrowserEntry{
		{name: "cfg", directory: true},
		{name: "zeta", directory: true},
		{name: "goose_pub.yaml"},
		{name: "PROJECT.LOG"},
	}
	if len(got) != len(want) {
		t.Fatalf("visible entries: got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestProjectRootPathMovesCfgSelectionToProjectRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if got := projectRootPath(filepath.Join(root, "cfg")); got != root {
		t.Fatalf("cfg selection: got root %q, want %q", got, root)
	}
	if got := projectRootPath(root); got != root {
		t.Fatalf("project selection: got root %q, want %q", got, root)
	}
}

func TestDefaultGooseSubscriberExposesAllFilterSettings(t *testing.T) {
	subscription := mustDefaultConfigItemTemplate(t, moduleGooseSub)
	wantKeys := []string{
		"name",
		"dst_mac",
		"src_mac",
		"app_id",
		"gocb_ref",
		"dat_set",
		"go_id",
		"conf_rev",
		"vlan_id",
		"vlan_pri",
		"accept_test",
		"accept_simulation",
		"accept_nds_com",
	}
	for _, key := range wantKeys {
		if mappingValue(subscription, key) == nil {
			t.Errorf("default GOOSE subscriber does not expose %q", key)
		}
	}
}

func TestPTPSourceSelectorAndLocalFields(t *testing.T) {
	root := mappingNode("enabled", scalarBool(true), "interface", scalarString("eth0"), "profile", scalarString("default"))
	ensurePTPSourceConfig(root, modulePTPClient)
	ensurePTPSourceConfig(root, modulePTPClient)
	if len(root.Content) != 8 || mappingValue(root, "source").Value != "external" {
		t.Fatal("source was not added exactly once with External default")
	}
	changed := 0
	selectBox := scalarWidget("source", mappingValue(root, "source"), func() { changed++ }).(*widget.Select)
	if selectBox.Selected != "External" || humanConfigLabel("source") != "Source PTP" {
		t.Fatalf("unexpected source selector: %+v", selectBox)
	}
	selectBox.SetSelected("Local")
	if mappingValue(root, "source").Value != "local" || changed != 1 {
		t.Fatal("Local selection was not stored")
	}
	s := &uiState{}
	content := s.yamlMappingEditor(root, func() {}).(*fyne.Container)
	form := content.Objects[0].(*widget.Form)
	if len(form.Items) != 2 {
		t.Fatalf("Local should show only enabled/source, got %d fields", len(form.Items))
	}
	selectBox.SetSelected("External")
	content = s.yamlMappingEditor(root, func() {}).(*fyne.Container)
	form = content.Objects[0].(*widget.Form)
	if len(form.Items) != 4 || mappingValue(root, "interface").Value != "eth0" {
		t.Fatal("External settings were lost during Local selection")
	}
}

func TestGooseSubscriberConfRevCanBeClearedAsOptionalFilter(t *testing.T) {
	node := scalarInt("7")
	setScalarValue("conf_rev", node, "")
	if node.Tag != "!!null" || node.Value != "" {
		t.Fatalf("cleared conf_rev: tag=%q value=%q", node.Tag, node.Value)
	}
}

func TestDefaultSVPublisherSupportsICDExport(t *testing.T) {
	stream := mustDefaultConfigItemTemplate(t, moduleSVPub)
	if !isSVPublisherMapping(stream) {
		t.Fatal("default SV stream was not recognized as an ICD-exportable publisher")
	}
	if dataset := mappingValue(stream, "dat_set"); dataset == nil || dataset.Value != "PhsMeas1" {
		t.Fatalf("default SV dataset: want PhsMeas1, got %#v", dataset)
	}
	if got := svSCLFileName("PBMT SV"); got != "PBMT_SV.icd" {
		t.Fatalf("SV ICD filename: got %q", got)
	}

	subscriber := mustDefaultConfigItemTemplate(t, moduleSVSub)
	if isSVPublisherMapping(subscriber) {
		t.Fatal("SV subscriber was incorrectly recognized as an ICD-exportable publisher")
	}
}

func TestAddConfigItemUsesEmbeddedTemplateWithUniqueIdentity(t *testing.T) {
	tests := []struct {
		id      moduleID
		key     string
		wantDst string
	}{
		{id: moduleGooseSub, key: "subscriptions", wantDst: "01:0c:cd:01:00:02"},
		{id: moduleGoosePub, key: "publishers", wantDst: "01:0c:cd:01:00:02"},
		{id: moduleSVSub, key: "subscriptions", wantDst: "01:0c:cd:04:00:02"},
		{id: moduleSVPub, key: "streams", wantDst: "01:0c:cd:04:00:02"},
	}

	for _, tc := range tests {
		t.Run(string(tc.id), func(t *testing.T) {
			existing := mustDefaultConfigItemTemplate(t, tc.id)
			root := mappingNode(tc.key, sequenceNode(existing))
			if err := addConfigItem(root, tc.id, tc.key, profiles.DefaultItemTemplate); err != nil {
				t.Fatal(err)
			}
			sequence := mappingSequence(root, tc.key)
			if len(sequence.Content) != 2 {
				t.Fatalf("items: got %d, want 2", len(sequence.Content))
			}

			added := sequence.Content[1]
			baseName := mappingValue(existing, "name").Value
			if got := mappingValue(added, "name").Value; got != baseName+"_2" {
				t.Fatalf("unique name: got %q, want %q", got, baseName+"_2")
			}
			if got := mappingValue(added, "dst_mac").Value; got != tc.wantDst {
				t.Fatalf("unique destination MAC: got %q, want %q", got, tc.wantDst)
			}
			want := mustDefaultConfigItemTemplate(t, tc.id)
			mappingValue(want, "name").Value = baseName + "_2"
			mappingValue(want, "dst_mac").Value = tc.wantDst
			wantYAML, err := yaml.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			gotYAML, err := yaml.Marshal(added)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotYAML, wantYAML) {
				t.Fatalf("added item differs from embedded template:\nwant:\n%s\ngot:\n%s", wantYAML, gotYAML)
			}

			mappingValue(added, "name").Value = "changed"
			if got := mappingValue(existing, "name").Value; got != baseName {
				t.Fatalf("template node was shared with existing item: got name %q", got)
			}
		})
	}
}

func TestAddConfigItemUsesFirstAvailableTemplateName(t *testing.T) {
	template := mustDefaultConfigItemTemplate(t, moduleSVPub)
	baseName := mappingValue(template, "name").Value
	third := mustDefaultConfigItemTemplate(t, moduleSVPub)
	mappingValue(third, "name").Value = baseName + "_3"
	mappingValue(third, "dst_mac").Value = "01:0c:cd:04:00:03"
	root := mappingNode("streams", sequenceNode(template, third))

	if err := addConfigItem(root, moduleSVPub, "streams", profiles.DefaultItemTemplate); err != nil {
		t.Fatal(err)
	}
	added := mappingSequence(root, "streams").Content[2]
	if got := mappingValue(added, "name").Value; got != baseName+"_2" {
		t.Fatalf("name collision handling: got %q, want %q", got, baseName+"_2")
	}
	if got := mappingValue(added, "dst_mac").Value; got != "01:0c:cd:04:00:02" {
		t.Fatalf("MAC collision handling: got %q, want %q", got, "01:0c:cd:04:00:02")
	}
}

func TestAddConfigItemDoesNotMutateOnTemplateError(t *testing.T) {
	root := mappingNode("streams", sequenceNode())
	load := func(string) ([]byte, error) { return nil, errors.New("unavailable") }
	if err := addConfigItem(root, moduleSVPub, "streams", load); err == nil {
		t.Fatal("template loader error was ignored")
	}
	if got := len(mappingSequence(root, "streams").Content); got != 0 {
		t.Fatalf("sequence mutated after template error: %d items", got)
	}
}

func mustDefaultConfigItemTemplate(t *testing.T, id moduleID) *yaml.Node {
	t.Helper()
	data, err := profiles.DefaultItemTemplate(moduleConfigFile(id))
	if err != nil {
		t.Fatal(err)
	}
	item, err := parseConfigItemTemplate(data)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestGooseDatasetDisplayEntriesPreservesTopLevelValuesAndNestedTree(t *testing.T) {
	timestamp := time.Date(2026, time.August, 5, 12, 34, 56, 123000000, time.FixedZone("UTC+3", 3*60*60))
	values := []goose.DataValue{
		{Type: goose.DataTypeBoolean, Bool: true},
		{Type: goose.DataTypeBitString, Bytes: []byte{0}},
		{Type: goose.DataTypeUTCTime, Time: timestamp},
		{Type: goose.DataTypeStructure, Children: []goose.DataValue{
			{Type: goose.DataTypeUnsigned, UInt: 7},
			{Type: goose.DataTypeArray, Children: []goose.DataValue{
				{Type: goose.DataTypeVisibleString, String: "ready"},
				{Type: goose.DataTypeStructure, Children: []goose.DataValue{
					{Type: goose.DataTypeBoolean, Bool: false},
				}},
			}},
			{Type: goose.DataTypeArray},
		}},
	}

	entries := gooseDatasetDisplayEntries(values)
	if len(entries) != len(values) {
		t.Fatalf("display entries: got %d, want %d: %+v", len(entries), len(values), entries)
	}
	if entries[0].Name != "Entry1" || entries[0].Summary != "true" {
		t.Fatalf("first entry: %+v", entries[0])
	}
	assertGooseDatasetNode(t, entries[0].Value, "Value", "BOOLEAN", "true", 0)
	assertGooseDatasetNode(t, entries[1].Value, "Value", "BIT_STRING", "0x00", 0)
	assertGooseDatasetNode(t, entries[2].Value, "Value", "UTC_TIME", "2026-08-05 12:34:56.123 +03:00", 0)

	object := entries[3]
	if object.Name != "Entry4" || object.Summary != "3 elements" {
		t.Fatalf("object entry: %+v", object)
	}
	assertGooseDatasetNode(t, object.Value, "Value", "STRUCTURE", "3 elements", 3)
	assertGooseDatasetNode(t, object.Value.Children[0], "Field 1", "UNSIGNED", "7", 0)
	array := object.Value.Children[1]
	assertGooseDatasetNode(t, array, "Field 2", "ARRAY", "2 elements", 2)
	assertGooseDatasetNode(t, array.Children[0], "Element 1", "VISIBLE_STRING", `"ready"`, 0)
	nested := array.Children[1]
	assertGooseDatasetNode(t, nested, "Element 2", "STRUCTURE", "1 elements", 1)
	assertGooseDatasetNode(t, nested.Children[0], "Field 1", "BOOLEAN", "false", 0)
	assertGooseDatasetNode(t, object.Value.Children[2], "Field 3", "ARRAY", "0 elements", 0)

	nodes, children := indexGooseDatasetTree(object.Value.Children)
	if len(nodes) != 6 {
		t.Fatalf("indexed nodes: got %d, want 6: %+v", len(nodes), nodes)
	}
	if got := nodes["2.2.1"]; got.Type != "BOOLEAN" || got.Value != "false" {
		t.Fatalf("deep node 2.2.1: %+v", got)
	}
	if got := children["2.2"]; len(got) != 1 || got[0] != "2.2.1" {
		t.Fatalf("deep children: %+v", got)
	}
	if got, branch := children["3"]; !branch || len(got) != 0 {
		t.Fatalf("empty array must remain a branch: children=%+v branch=%t", got, branch)
	}
}

func TestManualModuleActionState(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		supported bool
		wantStart bool
		wantStop  bool
	}{
		{name: "stopped", status: "Stopped", supported: true, wantStart: true},
		{name: "running", status: "Running", supported: true, wantStop: true},
		{name: "error", status: "Error", supported: true, wantStart: true},
		{name: "unsupported", status: "Stopped", supported: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := manualModuleActionState(tc.status, tc.supported)
			if got.startEnabled != tc.wantStart || got.stopEnabled != tc.wantStop {
				t.Fatalf("state: got %+v, want start=%t stop=%t", got, tc.wantStart, tc.wantStop)
			}
		})
	}
}

func TestRestoreAppTabIndex(t *testing.T) {
	tabs := container.NewAppTabs(
		container.NewTabItem("One", widget.NewLabel("one")),
		container.NewTabItem("Two", widget.NewLabel("two")),
		container.NewTabItem("Three", widget.NewLabel("three")),
	)
	restoreAppTabIndex(tabs, 2)
	if got := selectedAppTabIndex(tabs); got != 2 {
		t.Fatalf("selected tab: want 2, got %d", got)
	}
	restoreAppTabIndex(tabs, 99)
	if got := selectedAppTabIndex(tabs); got != 2 {
		t.Fatalf("invalid restore changed selected tab: got %d", got)
	}
}

func TestPTPServerIsBackedByRuntime(t *testing.T) {
	id, ok := runtimeModuleID(modulePTPServer)
	if !ok || id != pbmtruntime.ModulePTPServer {
		t.Fatalf("PTP Server runtime mapping: id=%q ok=%t", id, ok)
	}
}

func TestPTPClientIsBackedByRuntime(t *testing.T) {
	id, ok := runtimeModuleID(modulePTPClient)
	if !ok || id != pbmtruntime.ModulePTPClient {
		t.Fatalf("PTP Client runtime mapping: id=%q ok=%t", id, ok)
	}
}

func TestEnsurePTPDelayMechanismConfigMigratesOlderProjects(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    string
	}{
		{profile: "default", want: "e2e"},
		{profile: "power", want: "p2p"},
	} {
		root := mappingNode(
			"enabled", scalarBool(true),
			"profile", scalarString(tc.profile),
			"transport", scalarString("ethernet"),
		)
		ensurePTPDelayMechanismConfig(root, modulePTPClient)
		if got := mappingValue(root, "delay_mechanism"); got == nil || got.Value != tc.want {
			t.Errorf("profile %q: want %q, got %#v", tc.profile, tc.want, got)
		}
	}

	root := mappingNode(
		"profile", scalarString("default"),
		"delay_mechanism", scalarString("p2p"),
	)
	ensurePTPDelayMechanismConfig(root, modulePTPServer)
	if got := mappingValue(root, "delay_mechanism"); got == nil || got.Value != "p2p" {
		t.Fatalf("existing mechanism was replaced: %#v", got)
	}
}

func TestEnsurePTPPowerProfileConfigMigratesOlderProjects(t *testing.T) {
	root := mappingNode(
		"enabled", scalarBool(true),
		"profile", scalarString("power"),
	)
	ensurePTPPowerProfileConfig(root, modulePTPServer)
	power := mappingValue(root, "power_profile")
	if power == nil || power.Kind != yaml.MappingNode {
		t.Fatalf("power_profile mapping was not added: %#v", power)
	}
	if version := mappingValue(power, "version"); version == nil || version.Value != "2011" {
		t.Fatalf("C37.238 version: want 2011, got %#v", version)
	}
	if grandmasterID := mappingValue(power, "grandmaster_id"); grandmasterID == nil || grandmasterID.Value != "3" {
		t.Fatalf("Grandmaster ID: want 3, got %#v", grandmasterID)
	}
	alternate := mappingValue(power, "alternate_time_offset")
	if alternate == nil || mappingValue(alternate, "display_name").Value != "UTC+03:00" {
		t.Fatalf("alternate_time_offset preset is incomplete: %#v", alternate)
	}
}

func TestEnsurePTPClientPowerProfileConfigMigratesOlderProjects(t *testing.T) {
	root := mappingNode(
		"enabled", scalarBool(true),
		"profile", scalarString("power"),
	)
	ensurePTPPowerProfileConfig(root, modulePTPClient)
	power := mappingValue(root, "power_profile")
	if power == nil || power.Kind != yaml.MappingNode {
		t.Fatalf("power_profile mapping was not added: %#v", power)
	}
	if version := mappingValue(power, "version"); version == nil || version.Value != "2011" {
		t.Fatalf("C37.238 version: want 2011, got %#v", version)
	}
	if grandmasterID := mappingValue(power, "grandmaster_id"); grandmasterID != nil {
		t.Fatalf("client must not expose server transmit fields: %#v", grandmasterID)
	}
}

func TestEnsurePTPTraceabilityConfigMigratesOlderServerProjects(t *testing.T) {
	root := mappingNode(
		"profile", scalarString("power"),
		"time_traceable", scalarBool(true),
	)
	ensurePTPTraceabilityConfig(root, modulePTPServer)
	if got := mappingValue(root, "time_traceable"); got == nil || got.Value != "true" {
		t.Fatalf("existing time_traceable was replaced: %#v", got)
	}
	if got := mappingValue(root, "frequency_traceable"); got == nil || got.Value != "false" {
		t.Fatalf("missing frequency_traceable was not added as false: %#v", got)
	}

	client := mappingNode("profile", scalarString("power"))
	ensurePTPTraceabilityConfig(client, modulePTPClient)
	if mappingValue(client, "time_traceable") != nil || mappingValue(client, "frequency_traceable") != nil {
		t.Fatal("traceability fields were added to PTP Client")
	}
}

func TestLegacyPTPTimestampModeIsRemovedFromEditor(t *testing.T) {
	for _, id := range []moduleID{modulePTPClient, modulePTPServer} {
		t.Run(string(id), func(t *testing.T) {
			root := mappingNode(
				"profile", scalarString("default"),
				"timestamp_mode", scalarString("software"),
				"transport", scalarString("ethernet"),
				"timestamp_mode", scalarString("hardware"),
			)
			removeLegacyPTPTimestampModeConfig(root, id)
			if got := mappingValue(root, "timestamp_mode"); got != nil {
				t.Fatalf("legacy timestamp_mode remains: %#v", got)
			}
			if mappingValue(root, "profile") == nil || mappingValue(root, "transport") == nil {
				t.Fatal("removing timestamp_mode damaged neighboring settings")
			}
		})
	}

	nonPTP := mappingNode("timestamp_mode", scalarString("software"))
	removeLegacyPTPTimestampModeConfig(nonPTP, moduleSVPub)
	if mappingValue(nonPTP, "timestamp_mode") == nil {
		t.Fatal("non-PTP configuration was changed")
	}
}

func TestPowerProfileConfigurationIsVisibleOnlyForPowerProfile(t *testing.T) {
	root := mappingNode(
		"profile", scalarString("default"),
		"power_profile", mappingNode("version", scalarString("2011")),
	)
	if configMappingVisible(root, "power_profile") {
		t.Fatal("power_profile settings must be hidden for Default Profile")
	}
	mappingValue(root, "profile").Value = "power"
	if !configMappingVisible(root, "power_profile") {
		t.Fatal("power_profile settings must be visible for Power Profile")
	}
	if !configMappingVisible(root, "alternate_time_offset") {
		t.Fatal("unrelated nested mappings must remain visible")
	}
}

func TestPowerProfileEditorForcesP2PAndEthernet(t *testing.T) {
	root := mappingNode(
		"profile", scalarString("power"),
		"delay_mechanism", scalarString("e2e"),
		"transport", scalarString("udp"),
	)
	(&uiState{}).yamlMappingEditor(root, nil)
	if mechanism := mappingValue(root, "delay_mechanism"); mechanism == nil || mechanism.Value != "p2p" {
		t.Fatalf("Power Profile mechanism: want p2p, got %#v", mechanism)
	}
	if transport := mappingValue(root, "transport"); transport == nil || transport.Value != "ethernet" {
		t.Fatalf("Power Profile transport: want ethernet, got %#v", transport)
	}
}

func TestPTPProfileSelectionRefreshesConditionalConfiguration(t *testing.T) {
	node := scalarString("default")
	refreshCount := 0
	selectBox, ok := configSelect("profile", node, []string{"default", "power"}, func() {
		refreshCount++
	}).(*widget.Select)
	if !ok {
		t.Fatal("profile editor is not a select widget")
	}
	if refreshCount != 0 {
		t.Fatalf("initial selection unexpectedly refreshed the editor %d times", refreshCount)
	}
	selectBox.SetSelected("power")
	if node.Value != "power" || refreshCount != 1 {
		t.Fatalf("profile change: value=%q refreshes=%d, want power/1", node.Value, refreshCount)
	}
}

func TestPTPClientRowsExposeApplicationClock(t *testing.T) {
	domain := uint8(7)
	state := &uiState{cfg: &config.Config{PTPClient: config.PTPClientCfg{
		Interface:      "missing-test-interface",
		Profile:        "default",
		Transport:      "udp",
		DomainNumber:   &domain,
		DelayMechanism: "p2p",
	}}}
	rows := state.ptpClientRows()
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row[0]] = row[1]
	}
	for key, want := range map[string]string{
		"PTP profile":               "default",
		"Delay mechanism":           "P2P",
		"Domain number":             "7",
		"Timestamp":                 "auto",
		"Clock source for GOOSE/SV": "System",
	} {
		if values[key] != want {
			t.Errorf("%s: want %q, got %q", key, want, values[key])
		}
	}
}

func TestPTPClientRowsExposeExpectedPowerProfileVersion(t *testing.T) {
	state := &uiState{cfg: &config.Config{PTPClient: config.PTPClientCfg{
		Interface:      "missing-test-interface",
		Profile:        "power",
		Transport:      "ethernet",
		DelayMechanism: "p2p",
		PowerProfile:   config.PTPClientPowerProfileCfg{Version: "2017"},
	}}}
	values := make(map[string]string)
	for _, row := range state.ptpClientRows() {
		values[row[0]] = row[1]
	}
	if got := values["Expected C37.238 TLV"]; got != "enabled (version 2017, OUI 1C:12:9D, subtype 0x000002)" {
		t.Fatalf("expected C37.238 TLV: got %q", got)
	}
	if got := values["Master C37.238 TLV"]; got != "waiting for compatible Announce" {
		t.Fatalf("master C37.238 TLV: got %q", got)
	}
}

func TestPTPServerRowsExposeGrandmasterParameters(t *testing.T) {
	utcOffset := int16(37)
	priority1 := uint8(10)
	priority2 := uint8(20)
	clockClass := uint8(6)
	clockAccuracy := uint8(0x20)
	grandmasterID := uint16(3)
	totalTimeInaccuracy := uint32(25)
	timeTraceable := true
	frequencyTraceable := false
	state := &uiState{cfg: &config.Config{PTPServer: config.PTPServerCfg{
		Interface:          "missing-test-interface",
		Profile:            "power",
		Transport:          "ethernet",
		DelayMechanism:     "e2e",
		UTCOffset:          &utcOffset,
		TimeSource:         "gps",
		Priority1:          &priority1,
		Priority2:          &priority2,
		ClockClass:         &clockClass,
		ClockAccuracy:      &clockAccuracy,
		TimeTraceable:      &timeTraceable,
		FrequencyTraceable: &frequencyTraceable,
		PowerProfile: config.PTPPowerProfileCfg{
			Version:             "2017",
			GrandmasterID:       &grandmasterID,
			TotalTimeInaccuracy: &totalTimeInaccuracy,
		},
	}}}
	rows := state.ptpServerRows()
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row[0]] = row[1]
	}
	for key, want := range map[string]string{
		"PTP profile":                "power",
		"Delay mechanism":            "P2P",
		"Two step":                   "true",
		"Current UTC offset":         "37 s",
		"UTC offset valid":           "true",
		"Priority 1":                 "10",
		"Priority 2":                 "20",
		"Grandmaster clock class":    "Primary reference (synchronized) (6 / 0x06)",
		"Grandmaster clock accuracy": "25 ns (0x20)",
		"Offset scaled log variance": "Specified (0x4E5D)",
		"transportSpecific":          "0 (0x0)",
		"Sync interval":              "1s (log=0)",
		"Announce interval":          "2s (log=1)",
		"C37.238 TLV":                "enabled (version 2017, OUI 1C:12:9D, subtype 0x000002)",
		"C37.238 Grandmaster ID":     "3 (0x0003)",
		"Total time inaccuracy":      "25 ns",
		"Alternate time offset TLV":  "enabled (type 0x0009)",
		"Time source":                "GPS (0x20)",
		"Time traceable":             "true",
		"Frequency traceable":        "false",
		"Timestamp":                  "hardware",
	} {
		if values[key] != want {
			t.Errorf("%s: want %q, got %q", key, want, values[key])
		}
	}
}

func TestPTPServerRowsUseInteroperablePowerProfileDefaults(t *testing.T) {
	state := &uiState{cfg: &config.Config{PTPServer: config.PTPServerCfg{
		Interface:      "missing-test-interface",
		Profile:        "power",
		DelayMechanism: "p2p",
		Transport:      "ethernet",
		TimeSource:     "gps",
	}}}
	values := make(map[string]string)
	for _, row := range state.ptpServerRows() {
		values[row[0]] = row[1]
	}
	for key, want := range map[string]string{
		"transportSpecific":           "0 (0x0)",
		"Grandmaster clock class":     "Primary reference (synchronized) (6 / 0x06)",
		"Grandmaster clock accuracy":  "100 ns (0x21)",
		"Offset scaled log variance":  "Specified (0x4E5D)",
		"Time source":                 "GPS (0x20)",
		"Time traceable":              "true",
		"Frequency traceable":         "true",
		"C37.238 TLV":                 "enabled (version 2011, OUI 1C:12:9D, subtype 0x000001)",
		"C37.238 Grandmaster ID":      "3 (0x0003)",
		"Grandmaster time inaccuracy": "60 ns",
		"Network time inaccuracy":     "0 ns",
		"Total time inaccuracy":       "not transmitted",
		"Alternate time offset TLV":   "enabled (type 0x0009)",
		"Alternate current offset":    "10763 s",
		"Alternate display name":      "UTC+03:00",
	} {
		if values[key] != want {
			t.Errorf("%s: want %q, got %q", key, want, values[key])
		}
	}
}

func TestGooseDatasetAccordionPreservesExpandedEntries(t *testing.T) {
	accordion := widget.NewAccordion()
	empty := widget.NewLabel("empty")
	panel := &gooseSubscriberPanel{
		dataset:   container.NewVBox(empty, accordion),
		accordion: accordion,
		empty:     empty,
	}
	entries := []gooseDatasetDisplayEntry{{
		Name:    "Entry1",
		Summary: "false",
		Value:   gooseDatasetDisplayNode{Name: "Value", Type: "BOOLEAN", Value: "false"},
	}}
	updateGooseSubscriberDataset(panel, entries)
	accordion.Open(0)

	entries[0].Summary = "true"
	entries[0].Value.Value = "true"
	updateGooseSubscriberDataset(panel, entries)
	if len(accordion.Items) != 1 || !accordion.Items[0].Open {
		t.Fatalf("expanded state was not preserved: %+v", accordion.Items)
	}
	if accordion.Items[0].Title != "Entry1: true" {
		t.Fatalf("updated title: got %q, want %q", accordion.Items[0].Title, "Entry1: true")
	}
}

func TestGooseDatasetEntryDetailsContainVisibleValues(t *testing.T) {
	details := newGooseDatasetEntryDetails(gooseDatasetDisplayNode{
		Name:  "Value",
		Type:  "BOOLEAN",
		Value: "true",
	}, make(map[string]bool))
	if len(details.Objects) != 2 {
		t.Fatalf("detail rows: got %d, want 2", len(details.Objects))
	}
	row, ok := details.Objects[1].(*fyne.Container)
	if !ok || len(row.Objects) != 3 {
		t.Fatalf("value row is not a three-column Fyne container: %T", details.Objects[1])
	}
	if got := row.Objects[2].(*widget.Label).Text; got != "true" {
		t.Fatalf("visible value: got %q, want %q", got, "true")
	}
	if details.MinSize().Height <= 0 || row.MinSize().Height <= 0 {
		t.Fatalf("detail has no visible height: details=%v row=%v", details.MinSize(), row.MinSize())
	}
}

func TestGooseDataValueTextFormatsRawValues(t *testing.T) {
	if got := gooseDataValueText(goose.DataValue{Type: goose.DataTypeBitString, Bytes: []byte{0xAA, 0x05}}); got != "0xAA05" {
		t.Fatalf("bit string: got %q, want %q", got, "0xAA05")
	}
	if got := gooseDataValueText(goose.DataValue{Type: goose.DataTypeFloatingPoint, Float: 12.5}); got != "12.5" {
		t.Fatalf("float: got %q, want %q", got, "12.5")
	}
}

func TestGoosePublisherValueValidation(t *testing.T) {
	entry := widget.NewEntry()
	entry.SetText("18446744073709551615")
	value, err := goosePublisherControlValue(goosePublisherDataControl{
		entry:   config.GooseDatasetEntry{Name: "counter", Type: "uint"},
		textVal: entry,
	})
	if err != nil || value.UInt != ^uint64(0) {
		t.Fatalf("uint64 value: got %+v, err=%v", value, err)
	}

	entry.SetText("NaN")
	_, err = goosePublisherControlValue(goosePublisherDataControl{
		entry:   config.GooseDatasetEntry{Name: "measurement", Type: "float"},
		textVal: entry,
	})
	if err == nil {
		t.Fatal("non-finite floating-point value was accepted")
	}
}

func TestQualityHexValidationUsesIEC61850Mask(t *testing.T) {
	entry := widget.NewEntry()
	entry.SetText("0x3FFF")
	if got, err := parseQualityHexEntry(entry); err != nil || got != sv.QualityMask {
		t.Fatalf("full quality mask: got 0x%X, err=%v", got, err)
	}
	entry.SetText("0x4000")
	if _, err := parseQualityHexEntry(entry); err == nil {
		t.Fatal("unsupported quality bit was accepted")
	}
}

func assertGooseDatasetNode(t *testing.T, got gooseDatasetDisplayNode, name, dataType, value string, children int) {
	t.Helper()
	if got.Name != name || got.Type != dataType || got.Value != value || len(got.Children) != children {
		t.Fatalf("node: got %+v, want name=%q type=%q value=%q children=%d", got, name, dataType, value, children)
	}
}
