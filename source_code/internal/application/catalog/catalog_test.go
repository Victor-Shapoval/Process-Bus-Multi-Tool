package catalog

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
)

func fixture(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()
	model := &sclmodel.Model{Endpoint: sclmodel.Endpoint{IP: "10.10.5.22", Port: 102}, Devices: []sclmodel.LogicalDevice{{Name: "IEDLD0", Nodes: []sclmodel.LogicalNode{{Name: "LLN0", Groups: []*sclmodel.Type{{Name: "ST", Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "Ind1", Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "stVal", Kind: "BOOLEAN"}}}}}}, DataSets: []sclmodel.DataSet{{Name: "events", Members: []string{"IEDLD0/LLN0.Ind1.stVal[ST]"}}}, GOOSE: []sclmodel.GooseControl{{Name: "events", DataSet: "IEDLD0/LLN0$events", MAC: "01-0C-CD-01-00-01", APPID: 3, ConfRev: 7}}}}}}}
	raw, _, err := sclmodel.ExportICD(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "10.10.5.22.icd"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		GooseSub:  config.GooseSubCfg{Enabled: true, Subscriptions: []config.GooseSubscriber{{Name: "events", GocbRef: "IEDLD0/LLN0$GO$events", DatSet: "IEDLD0/LLN0$events", AppID: 3}}},
		GoosePub:  config.GoosePubCfg{Publishers: []config.GoosePublisher{{Name: "go/out", Dataset: []config.GooseDatasetEntry{{Name: "Trip", Type: "bool"}, {Name: "Trip.q", Type: "quality"}}}}},
		SVSub:     config.SVSubCfg{Subscriptions: []config.SVSubscriber{{Name: "measurements", CurrentCoefficient: 100, VoltageCoefficient: 200}}},
		SVPub:     config.SVPubCfg{Streams: []config.SVPublisher{{Name: "injection"}}},
		PTPClient: config.PTPClientCfg{Enabled: true, Interface: "private-ptp-interface"},
	}
	return root, cfg
}

func TestCatalogFourModulesSignalsAndNoPTP(t *testing.T) {
	root, cfg := fixture(t)
	c, err := Generate(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Modules) != 4 || c.Mode != "description_only" {
		t.Fatal("incorrect scope")
	}
	if c.Modules[0].Direction != "input" || c.Modules[1].Direction != "output" {
		t.Fatal("incorrect direction")
	}
	st := c.Modules[0].Streams[0]
	if st.SchemaStatus != "described" || len(st.Signals) != 1 || st.Signals[0].Reference != "IEDLD0/LLN0.Ind1.stVal" || *st.Signals[0].EntryIndex != 0 {
		t.Fatalf("GOOSE schema: %+v", st)
	}
	if c.Modules[1].ConfiguredEnabled || len(c.Modules[1].Streams[0].Signals) != 2 {
		t.Fatal("disabled publisher incorrectly represented")
	}
	for _, m := range c.Modules[2:] {
		if len(m.Streams[0].Signals) != 8 {
			t.Fatal("incorrect SV channels")
		}
	}
	if c.Modules[3].Streams[0].Signals[0].Unit != "A" || c.Modules[3].Streams[0].Signals[4].Unit != "V" || c.Modules[3].Streams[0].Signals[0].Basis != "secondary" {
		t.Fatal("incorrect SV units")
	}
	if !strings.Contains(c.Modules[1].Streams[0].ID, "go%2Fout") {
		t.Fatal("unsafe stream ID")
	}
	raw, _ := json.Marshal(c)
	if c.SchemaVersion != 2 || strings.Contains(string(raw), "runtime_state") || strings.Contains(string(raw), `"size"`) {
		t.Fatal("obsolete catalog format")
	}
	if st.Signals[0].Name != "LLN0.Ind1.stVal" {
		t.Fatal("GOOSE display name was not shortened")
	}
	if q := c.Modules[1].Streams[0].Signals[1]; q.BitLength != 14 || q.Encoding == "" {
		t.Fatal("publisher Quality encoding not described")
	}
	if q := c.Modules[3].Streams[0].Signals[0].Fields[3]; q.BitLength != 32 || q.Encoding != "uint32_quality_bitmask" {
		t.Fatal("SV Quality encoding not described")
	}
	if strings.Contains(string(raw), "ptp_client") || strings.Contains(string(raw), "private-ptp-interface") {
		t.Fatal("PTP exported")
	}
	again, err := Generate(root, cfg)
	if err != nil || again.Revision != c.Revision {
		t.Fatal("unstable catalog revision")
	}
	cfg.GooseSub.Subscriptions[0].AcceptTest = true
	changed, _ := Generate(root, cfg)
	if changed.Revision == c.Revision {
		t.Fatal("configuration change did not invalidate revision")
	}
}

func TestCatalogTypeDimensionsAndArrayTemplates(t *testing.T) {
	v := gooseSignal(&sclmodel.Type{Name: "samples", Kind: "ARRAY", Size: 3, Children: []*sclmodel.Type{{Kind: "STRUCTURE", Children: []*sclmodel.Type{{Name: "q", Kind: "BIT_STRING", Size: -13}, {Name: "label", Kind: "VISIBLE_STRING", Size: -65}, {Name: "bytes", Kind: "OCTET_STRING", Size: -8}, {Name: "time", Kind: "BINARY_TIME", Size: 6}}}}}, "entry/0")
	if v.Count == nil || *v.Count != 3 || v.IsTemplate || v.BitLength != 0 {
		t.Fatal("array dimensions are ambiguous")
	}
	if !v.Children[0].IsTemplate {
		t.Fatal("element schema is not marked")
	}
	children := v.Children[0].Children
	for _, ch := range children {
		if !ch.IsTemplate {
			t.Fatal("template descendant appears addressable")
		}
	}
	if children[0].Type != "Quality" || children[0].BitLength != 13 || children[0].Encoding == "" {
		t.Fatal("input Quality width/encoding lost")
	}
	if children[1].MaxLength != 65 || children[1].LengthUnit != "characters" {
		t.Fatal("string length unclear")
	}
	if children[2].MaxLength != 8 || children[2].LengthUnit != "octets" || children[3].ByteLength != 6 {
		t.Fatal("byte dimensions unclear")
	}
}

func TestCatalogUnknownDatasetAndDuplicateNames(t *testing.T) {
	root, cfg := fixture(t)
	cfg.GooseSub.Subscriptions[0].GocbRef = "unmatched"
	c, err := Generate(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := c.Modules[0].Streams[0]
	if st.SchemaStatus != "unresolved" || len(st.Signals) != 0 || len(st.Notes) == 0 {
		t.Fatal("unknown DataSet invented")
	}
	cfg.GooseSub.Subscriptions = append(cfg.GooseSub.Subscriptions, cfg.GooseSub.Subscriptions[0])
	if _, err := Generate(root, cfg); err == nil {
		t.Fatal("duplicate identifiers accepted")
	}
}

func TestCatalogSaveReplacesOnlyDerivedFile(t *testing.T) {
	root, cfg := fixture(t)
	c, err := Generate(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(root, c); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed Catalog
	if err := json.Unmarshal(before, &parsed); err != nil || parsed.Revision != c.Revision {
		t.Fatal("invalid catalog file")
	}
	c.Modules[0].Streams[0].Metadata["invalid"] = math.NaN()
	if err := Save(root, c); err == nil {
		t.Fatal("bad catalog saved")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed save damaged previous file")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("extra files created")
	}
}

func TestGenerateCapturedProject(t *testing.T) {
	root := os.Getenv("PBMT_CATALOG_PROJECT")
	if root == "" {
		t.Skip("set PBMT_CATALOG_PROJECT for an existing project")
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Generate(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Modules {
		for _, s := range m.Streams {
			t.Logf("%s/%s: %s, %d signals", m.ID, s.Name, s.SchemaStatus, len(s.Signals))
		}
	}
	// The user's project is never written by this optional diagnostic test.
	if err := Save(t.TempDir(), c); err != nil {
		t.Fatal(err)
	}
}
