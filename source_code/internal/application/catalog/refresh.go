package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
)

// SourcesFingerprint excludes generated output and logs to prevent refresh
// loops. Content hashes detect same-size edits and atomic file replacement.
func SourcesFingerprint(root string) (string, error) {
	dir, err := config.ConfigDir(root)
	if err != nil {
		return "", err
	}
	paths := []string{}
	for _, name := range []string{"goose_sub.yaml", "goose_pub.yaml", "sv_sub.yaml", "sv_pub.yaml", "ptp_client.yaml", "ptp_server.yaml"} {
		// config.Load validates the whole project; these files are observed,
		// but PTP configuration is never exposed in the catalog.
		paths = append(paths, filepath.Join(dir, name))
	}
	names, err := sclmodel.ProjectICDFiles(root)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		paths = append(paths, filepath.Join(root, name))
	}
	hash := sha256.New()
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return "", fmt.Errorf("not a readable regular file: %s", filepath.Base(path))
		}
		fileHash := sha256.New()
		n, err := io.Copy(fileHash, io.LimitReader(f, (32<<20)+1))
		f.Close()
		if err != nil {
			return "", err
		}
		if n > 32<<20 {
			return "", fmt.Errorf("%s exceeds 32 MiB", filepath.Base(path))
		}
		fmt.Fprintf(hash, "%s\x00%x\x00", filepath.Base(path), fileHash.Sum(nil))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Refresh re-reads saved YAML/ICD, never applies settings to running modules,
// and persists an explicit unavailable marker on generation failure. The old
// inventory must not silently survive a source error as if it were current.
func Refresh(root string, applied *config.Config) (*Catalog, error) {
	c, generationErr := readSaved(root, applied)
	if generationErr != nil {
		c = &Catalog{SchemaVersion: 2, Project: filepath.Base(filepath.Clean(root)), Mode: "description_only", Status: "unavailable", ConfigurationState: "unknown", Notes: []string{generationErr.Error(), "No usable signal inventory. Correct the project files and refresh."}, Modules: []Module{}}
		if err := c.finalize(); err != nil {
			return nil, err
		}
	}
	if err := Save(root, c); err != nil {
		return c, fmt.Errorf("catalog file was NOT updated; any previous file may be stale: %w", err)
	}
	return c, generationErr
}

func readSaved(root string, applied *config.Config) (*Catalog, error) {
	before, err := SourcesFingerprint(root)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	c, err := Generate(root, cfg)
	if err != nil {
		return nil, err
	}
	after, err := SourcesFingerprint(root)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("project files changed during catalog generation; refresh again")
	}
	c.ConfigurationState = "not_compared"
	if applied != nil {
		digest, err := configurationDigest(applied)
		if err != nil {
			return nil, err
		}
		c.ConfigurationState = "matches_applied"
		if digest != c.ConfigurationDigest {
			c.ConfigurationState = "differs_from_applied"
			c.Notes = append(c.Notes, "Saved GOOSE/SV configuration differs from the applied configuration. No runtime settings were changed; reload/apply the project before using it for control.")
		}
	}
	return c, c.finalize()
}
