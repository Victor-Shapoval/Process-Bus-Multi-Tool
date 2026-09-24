package profiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"pbmt/internal/config"
)

func TestDefaultItemTemplatesMatchEmbeddedProfiles(t *testing.T) {
	tests := []struct {
		file string
		key  string
	}{
		{file: "goose_sub.yaml", key: "subscriptions"},
		{file: "goose_pub.yaml", key: "publishers"},
		{file: "sv_sub.yaml", key: "subscriptions"},
		{file: "sv_pub.yaml", key: "streams"},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			profileData, err := defaultFiles.ReadFile(filepath.ToSlash(filepath.Join("default", tc.file)))
			if err != nil {
				t.Fatal(err)
			}
			var profile yaml.Node
			if err := yaml.Unmarshal(profileData, &profile); err != nil {
				t.Fatal(err)
			}
			sequence := defaultMappingSequence(profile.Content[0], tc.key)
			if sequence == nil || len(sequence.Content) == 0 {
				t.Fatalf("embedded profile has no %s item", tc.key)
			}
			want, err := yaml.Marshal(sequence.Content[0])
			if err != nil {
				t.Fatal(err)
			}
			got, err := DefaultItemTemplate(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("template differs from embedded profile:\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

func TestDefaultItemTemplateRejectsUnknownProfile(t *testing.T) {
	for _, name := range []string{"ptp_client.yaml", "../goose_pub.yaml", "missing.yaml"} {
		if _, err := DefaultItemTemplate(name); err == nil {
			t.Fatalf("unknown profile %q was accepted", name)
		}
	}
}

func TestDefaultPTPTemplatesDoNotExposeTimestampMode(t *testing.T) {
	for _, name := range []string{"ptp_client.yaml", "ptp_server.yaml"} {
		t.Run(name, func(t *testing.T) {
			data, err := defaultFiles.ReadFile(filepath.ToSlash(filepath.Join("default", name)))
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]interface{}
			if err := yaml.Unmarshal(data, &values); err != nil {
				t.Fatal(err)
			}
			if _, exists := values["timestamp_mode"]; exists {
				t.Fatal("default PTP template exposes legacy timestamp_mode")
			}
		})
	}
}

func TestCreateProjectUsesEmbeddedValidatedTemplate(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := CreateProject(projectDir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cfg" || !entries[0].IsDir() {
		t.Fatalf("project entries: got %+v, want only cfg", entries)
	}
	if _, err := config.Load(projectDir); err != nil {
		t.Fatalf("load created project: %v", err)
	}
}

func TestCreateProjectDoesNotModifyExistingDestination(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(projectDir, "owned-by-user")
	if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := CreateProject(projectDir); err == nil {
		t.Fatal("expected existing destination error")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing destination was changed: data=%q err=%v", data, err)
	}
}

func TestDefaultPublisherAndSubscriberIdentitiesArePaired(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := CreateProject(projectDir); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	goosePub := cfg.GoosePub.Publishers[0]
	gooseSub := cfg.GooseSub.Subscriptions[0]
	if gooseSub.AppID != goosePub.AppID || gooseSub.GocbRef != goosePub.GocbRef || gooseSub.DatSet != goosePub.DatSet || gooseSub.GoID != goosePub.GoID {
		t.Fatalf("default GOOSE identities are not paired: publisher=%+v subscriber=%+v", goosePub, gooseSub)
	}
	svPub := cfg.SVPub.Streams[0]
	svSub := cfg.SVSub.Subscriptions[0]
	if svSub.AppID != svPub.AppID || svSub.SvID != svPub.SvID {
		t.Fatalf("default SV identities are not paired: publisher=%+v subscriber=%+v", svPub, svSub)
	}
	if svPub.DatSet != "PhsMeas1" {
		t.Fatalf("default SV publisher dataset: want PhsMeas1, got %q", svPub.DatSet)
	}
}
