// Package profiles provides the default project template embedded in the
// executable. Project creation therefore does not depend on the process
// working directory or files installed next to the binary.
package profiles

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"pbmt/internal/config"
)

//go:embed default/*.yaml
var defaultFiles embed.FS

var profileFileNames = []string{
	"goose_sub.yaml",
	"goose_pub.yaml",
	"sv_sub.yaml",
	"sv_pub.yaml",
	"ptp_client.yaml",
	"ptp_server.yaml",
}

var defaultItemSequenceKeys = map[string]string{
	"goose_sub.yaml": "subscriptions",
	"goose_pub.yaml": "publishers",
	"sv_sub.yaml":    "subscriptions",
	"sv_pub.yaml":    "streams",
}

// DefaultItemTemplate returns the first configured item from an embedded
// default GOOSE/SV profile. The GUI uses this as the single source of truth
// when adding publishers, subscribers, and streams to an existing project.
func DefaultItemTemplate(profileFile string) ([]byte, error) {
	sequenceKey, ok := defaultItemSequenceKeys[profileFile]
	if !ok {
		return nil, fmt.Errorf("default item template is not available for %q", profileFile)
	}
	data, err := defaultFiles.ReadFile(filepath.ToSlash(filepath.Join("default", profileFile)))
	if err != nil {
		return nil, fmt.Errorf("read embedded profile %q: %w", profileFile, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse embedded profile %q: %w", profileFile, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("embedded profile %q must contain a mapping", profileFile)
	}
	sequence := defaultMappingSequence(doc.Content[0], sequenceKey)
	if sequence == nil || len(sequence.Content) == 0 {
		return nil, fmt.Errorf("embedded profile %q has no %s template", profileFile, sequenceKey)
	}
	item := sequence.Content[0]
	if item.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("embedded profile %q %s template must be a mapping", profileFile, sequenceKey)
	}
	encoded, err := yaml.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("encode embedded profile %q %s template: %w", profileFile, sequenceKey, err)
	}
	return encoded, nil
}

func defaultMappingSequence(root *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key && root.Content[i+1].Kind == yaml.SequenceNode {
			return root.Content[i+1]
		}
	}
	return nil
}

// CreateProject writes a validated default project through a staging
// directory and publishes it with one rename. On failure no partial project is
// left at projectDir.
func CreateProject(projectDir string) error {
	projectDir = filepath.Clean(projectDir)
	parent := filepath.Dir(projectDir)
	base := filepath.Base(projectDir)
	if base == "." || base == string(filepath.Separator) {
		return fmt.Errorf("invalid project directory %q", projectDir)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("stat project parent %q: %w", parent, err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("project parent %q is not a directory", parent)
	}
	if _, err := os.Lstat(projectDir); err == nil {
		return fmt.Errorf("project directory already exists: %s", projectDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat project directory %q: %w", projectDir, err)
	}

	stagingDir, err := os.MkdirTemp(parent, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create project staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	if err := os.Chmod(stagingDir, 0755); err != nil {
		return fmt.Errorf("set project directory permissions: %w", err)
	}
	cfgDir := filepath.Join(stagingDir, "cfg")
	if err := os.Mkdir(cfgDir, 0755); err != nil {
		return fmt.Errorf("create project config directory: %w", err)
	}
	for _, name := range profileFileNames {
		data, err := defaultFiles.ReadFile(filepath.ToSlash(filepath.Join("default", name)))
		if err != nil {
			return fmt.Errorf("read embedded profile %q: %w", name, err)
		}
		if err := writeNewFile(filepath.Join(cfgDir, name), data); err != nil {
			return err
		}
	}
	if _, err := config.Load(stagingDir); err != nil {
		return fmt.Errorf("validate embedded project template: %w", err)
	}
	if err := syncDirectory(cfgDir); err != nil {
		return err
	}
	if err := syncDirectory(stagingDir); err != nil {
		return err
	}
	if err := os.Rename(stagingDir, projectDir); err != nil {
		return fmt.Errorf("publish project directory %q: %w", projectDir, err)
	}
	committed = true
	_ = syncDirectory(parent)
	return nil
}

func writeNewFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("create profile %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write profile %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync profile %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close profile %q: %w", path, err)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %q for sync: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync directory %q: %w", path, err)
	}
	return nil
}
