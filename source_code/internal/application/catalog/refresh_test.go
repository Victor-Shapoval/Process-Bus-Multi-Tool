package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pbmt/internal/config"
	"pbmt/profiles"
)

func savedFixture(t *testing.T) (string, *config.Config) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := profiles.CreateProject(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

func TestRefreshReadsSavedYAMLWithoutChangingAppliedConfig(t *testing.T) {
	root, applied := savedFixture(t)
	first, err := Refresh(root, applied)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigurationState != "matches_applied" {
		t.Fatal("matching configuration not identified")
	}
	path := filepath.Join(root, "cfg", "sv_pub.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	newName := applied.SVPub.Streams[0].Name + "_edited"
	raw = []byte(strings.Replace(string(raw), "name: "+applied.SVPub.Streams[0].Name, "name: "+newName, 1))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Refresh(root, applied)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigurationState != "differs_from_applied" || c.Modules[3].Streams[0].Name != newName || c.Revision == first.Revision {
		t.Fatal("disk changes ignored")
	}
	if applied.SVPub.Streams[0].Name == newName {
		t.Fatal("applied configuration was mutated")
	}
}

func TestRefreshFailureInvalidatesOldCatalogAndRecovers(t *testing.T) {
	root, cfg := savedFixture(t)
	if _, err := Refresh(root, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cfg", "goose_pub.yaml")
	original, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte("broken: ["), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Refresh(root, cfg)
	if err == nil || c.Status != "unavailable" || len(c.Modules) != 0 {
		t.Fatal("old inventory survives invalid source")
	}
	raw, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var saved Catalog
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Status != "unavailable" || len(saved.Modules) != 0 {
		t.Fatal("unavailable marker not persisted")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Refresh(root, cfg)
	if err != nil || c.Status == "unavailable" || len(c.Modules) != 4 {
		t.Fatal("catalog did not recover")
	}
}

func TestSourceFingerprintDetectsICDEditsButIgnoresGeneratedFiles(t *testing.T) {
	root, cfg := savedFixture(t)
	first, err := SourcesFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(root, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "project.log"), []byte("log"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := SourcesFingerprint(root)
	if err != nil || first != second {
		t.Fatal("output/log caused a refresh loop")
	}
	path := filepath.Join(root, "model.icd")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	third, _ := SourcesFingerprint(root)
	info, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	fourth, _ := SourcesFingerprint(root)
	if first == third || third == fourth {
		t.Fatal("same-size ICD modification not detected")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	last, _ := SourcesFingerprint(root)
	if first != last {
		t.Fatal("ICD removal not detected")
	}
}
