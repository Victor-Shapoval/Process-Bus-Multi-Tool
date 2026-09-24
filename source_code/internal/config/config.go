// Package config loads and validates a Process Bus Multi Tool profile.
package config

import (
	"bytes"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

// Config is the root configuration. Each module has its own section with an
// enabled flag; parameters are added as modules are implemented.
type Config struct {
	GooseSub  GooseSubCfg  `yaml:"goose_sub"`
	SVSub     SVSubCfg     `yaml:"sv_sub"`
	PTPClient PTPClientCfg `yaml:"ptp_client"`
	GoosePub  GoosePubCfg  `yaml:"goose_pub"`
	SVPub     SVPubCfg     `yaml:"sv_pub"`
	PTPServer PTPServerCfg `yaml:"ptp_server"`
}

// GooseSubCfg configures the GOOSE reception module.
type GooseSubCfg struct {
	Enabled       bool              `yaml:"enabled"`
	Interface     string            `yaml:"interface"`   // network interface for live capture
	Promiscuous   bool              `yaml:"promiscuous"` // promiscuous mode
	Subscriptions []GooseSubscriber `yaml:"subscriptions"`
}

// GooseSubscriber defines one GOOSE subscription.
type GooseSubscriber struct {
	Name             string  `yaml:"name"`
	DstMAC           string  `yaml:"dst_mac"`
	SrcMAC           string  `yaml:"src_mac"`
	AppID            HexU16  `yaml:"app_id"`
	MatchAnyAppID    bool    `yaml:"match_any_app_id"`
	AllowNonStandard bool    `yaml:"allow_nonstandard"`
	GocbRef          string  `yaml:"gocb_ref"`
	DatSet           string  `yaml:"dat_set"`
	GoID             string  `yaml:"go_id"`
	ConfRev          *uint32 `yaml:"conf_rev"`
	VLANID           *uint16 `yaml:"vlan_id"`
	VLANPri          *uint8  `yaml:"vlan_pri"`
	AcceptTest       bool    `yaml:"accept_test"`
	AcceptSimulation bool    `yaml:"accept_simulation"`
	AcceptNdsCom     bool    `yaml:"accept_nds_com"`
}

// HexU16 accepts hexadecimal (0x0001) and decimal (1) YAML values.
type HexU16 uint16

// UnmarshalYAML parses HexU16 from a YAML node.
func (h *HexU16) UnmarshalYAML(node *yaml.Node) error {
	s := strings.TrimSpace(node.Value)
	if s == "" {
		*h = 0
		return nil
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 16)
		if err != nil {
			return fmt.Errorf("HexU16: %q: %w", node.Value, err)
		}
		*h = HexU16(v)
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return fmt.Errorf("HexU16: %q: %w", node.Value, err)
	}
	*h = HexU16(v)
	return nil
}

// Load reads a configuration profile from a directory, applies defaults, and validates fields.
func Load(path string) (*Config, error) {
	dir, err := ConfigDir(path)
	if err != nil {
		return nil, err
	}
	return loadProfileDir(dir)
}

// ConfigDir returns the directory containing the YAML configuration files.
// For a working project this is <project>/cfg; for a template directory it is the directory itself.
func ConfigDir(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q must be a profile directory", path)
	}
	cfgDir := filepath.Join(path, "cfg")
	if cfgInfo, err := os.Stat(cfgDir); err == nil && cfgInfo.IsDir() {
		return cfgDir, nil
	}
	return path, nil
}

func loadProfileDir(dir string) (*Config, error) {
	return loadProfileDirWithCandidate(dir, "", nil)
}

func loadProfileDirWithCandidate(dir, candidateName string, candidate []byte) (*Config, error) {
	var c Config
	if err := readProfileYAML(dir, "goose_sub.yaml", candidateName, candidate, &c.GooseSub); err != nil {
		return nil, err
	}
	if err := readProfileYAML(dir, "goose_pub.yaml", candidateName, candidate, &c.GoosePub); err != nil {
		return nil, err
	}
	if err := readProfileYAML(dir, "sv_sub.yaml", candidateName, candidate, &c.SVSub); err != nil {
		return nil, err
	}
	if err := readProfileYAML(dir, "sv_pub.yaml", candidateName, candidate, &c.SVPub); err != nil {
		return nil, err
	}
	if err := readProfileYAML(dir, "ptp_client.yaml", candidateName, candidate, &c.PTPClient); err != nil {
		return nil, err
	}
	if err := readProfileYAML(dir, "ptp_server.yaml", candidateName, candidate, &c.PTPServer); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func readProfileYAML(dir, name, candidateName string, candidate []byte, out interface{}) error {
	path := filepath.Join(dir, name)
	data := candidate
	if name != candidateName {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %q: %w", path, err)
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse %q: %w", path, err)
	}
	return nil
}

// SaveProfileFile validates a candidate YAML file together with the rest of
// the project and only then atomically replaces the file on disk. The returned
// Config is the exact validated configuration that was committed.
func SaveProfileFile(profilePath, name string, data []byte) (*Config, error) {
	if !isProfileFile(name) {
		return nil, fmt.Errorf("unsupported profile file %q", name)
	}
	dir, err := ConfigDir(profilePath)
	if err != nil {
		return nil, err
	}
	cfg, err := loadProfileDirWithCandidate(dir, name, data)
	if err != nil {
		return nil, fmt.Errorf("validate candidate %q: %w", name, err)
	}

	target := filepath.Join(dir, name)
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q must be a regular file", target)
	}
	if err := atomicWriteFile(target, data, info.Mode().Perm()); err != nil {
		return nil, err
	}
	return cfg, nil
}

func isProfileFile(name string) bool {
	switch name {
	case "goose_sub.yaml", "goose_pub.yaml", "sv_sub.yaml", "sv_pub.yaml", "ptp_client.yaml", "ptp_server.yaml":
		return true
	default:
		return false
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.PTPClient.Source == "" {
		c.PTPClient.Source = "external"
	}
	if c.PTPClient.Profile == "" {
		c.PTPClient.Profile = "default"
	}
	if c.PTPClient.Profile == "power" {
		c.PTPClient.Transport = "ethernet"
	} else if c.PTPClient.Transport == "" {
		c.PTPClient.Transport = "udp"
	}
	// timestamp_mode is a legacy compatibility key only. Timestamp policy is
	// selected by the application and cannot be configured by a project.
	c.PTPClient.DeprecatedTimestampMode = ""
	if c.PTPClient.Profile == "power" {
		c.PTPClient.DelayMechanism = "p2p"
	} else if c.PTPClient.DelayMechanism == "" {
		c.PTPClient.DelayMechanism = "e2e"
	}
	if c.PTPClient.PowerProfile.Version == "" {
		c.PTPClient.PowerProfile.Version = "2011"
	}
	if c.PTPServer.Profile == "" {
		c.PTPServer.Profile = "default"
	}
	if c.PTPServer.Profile == "power" {
		c.PTPServer.Transport = "ethernet"
	} else if c.PTPServer.Transport == "" {
		c.PTPServer.Transport = "udp"
	}
	c.PTPServer.DeprecatedTimestampMode = ""
	if c.PTPServer.Profile == "power" {
		c.PTPServer.DelayMechanism = "p2p"
	} else if c.PTPServer.DelayMechanism == "" {
		c.PTPServer.DelayMechanism = "e2e"
	}
	if c.PTPServer.UTCOffset == nil {
		utcOffset := int16(37)
		c.PTPServer.UTCOffset = &utcOffset
	}
	if c.PTPServer.TimeTraceable == nil {
		value := false
		c.PTPServer.TimeTraceable = &value
	}
	if c.PTPServer.FrequencyTraceable == nil {
		value := false
		c.PTPServer.FrequencyTraceable = &value
	}
	applyPTPPowerProfileDefaults(&c.PTPServer.PowerProfile)
	for i := range c.SVSub.Subscriptions {
		sub := &c.SVSub.Subscriptions[i]
		if sub.BaseVector == "" {
			sub.BaseVector = "Ua"
		}
		if sub.SmpRate == 0 {
			sub.SmpRate = 80
		}
		if sub.SampleTimingFrequency == 0 {
			sub.SampleTimingFrequency = 50
		}
		if sub.CurrentCoefficient == 0 {
			sub.CurrentCoefficient = 1
		}
		if sub.VoltageCoefficient == 0 {
			sub.VoltageCoefficient = 1
		}
	}
}

func (c *Config) validate() error {
	if c.GooseSub.Enabled {
		if err := c.GooseSub.validate(); err != nil {
			return fmt.Errorf("goose_sub: %w", err)
		}
	}
	if c.SVSub.Enabled {
		if err := c.SVSub.validate(); err != nil {
			return fmt.Errorf("sv_sub: %w", err)
		}
	}
	if c.PTPClient.Enabled {
		if err := c.PTPClient.validate(); err != nil {
			return fmt.Errorf("ptp_client: %w", err)
		}
	}
	if c.GoosePub.Enabled {
		if err := c.GoosePub.validate(); err != nil {
			return fmt.Errorf("goose_pub: %w", err)
		}
	}
	if c.SVPub.Enabled {
		if err := c.SVPub.validate(); err != nil {
			return fmt.Errorf("sv_pub: %w", err)
		}
	}
	if c.PTPServer.Enabled {
		if err := c.PTPServer.validate(); err != nil {
			return fmt.Errorf("ptp_server: %w", err)
		}
	}
	return nil
}

func (g *GooseSubCfg) validate() error {
	if g.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if len(g.Subscriptions) == 0 {
		return fmt.Errorf("at least one subscription is required")
	}
	seen := make(map[string]bool, len(g.Subscriptions))
	for i := range g.Subscriptions {
		sub := &g.Subscriptions[i]
		if sub.Name == "" {
			return fmt.Errorf("subscription #%d: name is required", i)
		}
		if seen[sub.Name] {
			return fmt.Errorf("subscription %q: duplicate name", sub.Name)
		}
		seen[sub.Name] = true
		if sub.DstMAC == "" {
			return fmt.Errorf("subscription %q: dst_mac is required", sub.Name)
		}
		dstMAC, err := parseMAC48(sub.DstMAC)
		if err != nil {
			return fmt.Errorf("subscription %q: invalid dst_mac %q: %w", sub.Name, sub.DstMAC, err)
		}
		if !sub.AllowNonStandard && !goose.IsValidMulticastMAC(dstMAC) {
			return fmt.Errorf("subscription %q: dst_mac %q is outside the IEC 61850 GOOSE multicast range", sub.Name, sub.DstMAC)
		}
		if sub.SrcMAC != "" {
			if _, err := parseMAC48(sub.SrcMAC); err != nil {
				return fmt.Errorf("subscription %q: invalid src_mac %q: %w", sub.Name, sub.SrcMAC, err)
			}
		}
		if !sub.MatchAnyAppID && !sub.AllowNonStandard && !goose.IsValidAppID(uint16(sub.AppID)) {
			return fmt.Errorf("subscription %q: app_id 0x%04X is outside the IEC 61850 GOOSE range", sub.Name, uint16(sub.AppID))
		}
		if sub.VLANID != nil && *sub.VLANID > 4094 {
			return fmt.Errorf("subscription %q: vlan_id must be 0..4094", sub.Name)
		}
		if sub.VLANPri != nil && *sub.VLANPri > 7 {
			return fmt.Errorf("subscription %q: vlan_pri must be 0..7", sub.Name)
		}
		if sub.VLANPri != nil && sub.VLANID == nil {
			return fmt.Errorf("subscription %q: vlan_id is required when vlan_pri is set", sub.Name)
		}
	}
	return nil
}

// SVSubCfg configures the SV reception module (IEC 61850-9-2LE).
type SVSubCfg struct {
	Enabled       bool           `yaml:"enabled"`
	Interface     string         `yaml:"interface"` // network interface (required)
	Promiscuous   bool           `yaml:"promiscuous"`
	Subscriptions []SVSubscriber `yaml:"subscriptions"`
}

// SVSubscriber defines one SV subscription.
type SVSubscriber struct {
	Name                  string  `yaml:"name"`
	DstMAC                string  `yaml:"dst_mac"`
	SrcMAC                string  `yaml:"src_mac"`
	AppID                 HexU16  `yaml:"app_id"`
	MatchAnyAppID         bool    `yaml:"match_any_app_id"`
	AllowNonStandard      bool    `yaml:"allow_nonstandard"`
	SvID                  string  `yaml:"sv_id"`
	VLANID                *uint16 `yaml:"vlan_id"`
	SmpRate               uint16  `yaml:"smp_rate"`                // samples per period (80 or 256)
	SampleTimingFrequency uint16  `yaml:"sample_timing_frequency"` // nominal grid frequency (50 or 60 Hz)
	BaseVector            string  `yaml:"base_vector"`             // reference vector for phasor display
	CurrentCoefficient    float64 `yaml:"current_coefficient"`     // KI, engineering current coefficient
	VoltageCoefficient    float64 `yaml:"voltage_coefficient"`     // KU, engineering voltage coefficient
}

// PTPClientCfg configures the PTP client module (IEEE 1588-2008).
type PTPClientCfg struct {
	Enabled   bool   `yaml:"enabled"`
	Source    string `yaml:"source"`    // "local" binds to PBMT's server; "external" uses the network
	Interface string `yaml:"interface"` // network interface (required)
	Profile   string `yaml:"profile"`   // "default" or "power" (IEEE C37.238)
	Transport string `yaml:"transport"` // "udp" or "ethernet"
	// DeprecatedTimestampMode only lets projects created by older releases
	// load successfully. Its value is discarded by applyDefaults.
	DeprecatedTimestampMode string                   `yaml:"timestamp_mode,omitempty"`
	DomainNumber            *uint8                   `yaml:"domain_number"`   // nil uses the profile value
	DelayMechanism          string                   `yaml:"delay_mechanism"` // "e2e" or "p2p"; omitted uses the profile value
	PowerProfile            PTPClientPowerProfileCfg `yaml:"power_profile"`   // expected IEEE C37.238 wire version
}

// PTPClientPowerProfileCfg selects the C37.238 Organization Extension TLV
// version that a Power Profile grandmaster must advertise.
type PTPClientPowerProfileCfg struct {
	Version string `yaml:"version"`
}

func (p *PTPClientCfg) validate() error {
	switch p.Source {
	case "local":
		// Network settings are retained but unused by the internal binding.
		return nil
	case "", "external":
	default:
		return fmt.Errorf("source must be local|external, got %q", p.Source)
	}
	if p.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	switch p.Profile {
	case "", "default", "power":
	default:
		return fmt.Errorf("profile must be default|power, got %q", p.Profile)
	}
	switch p.Transport {
	case "", "udp", "ethernet":
	default:
		return fmt.Errorf("transport must be udp|ethernet, got %q", p.Transport)
	}
	if p.Profile == "power" && p.Transport != "ethernet" {
		return fmt.Errorf("power profile requires ethernet transport")
	}
	if err := validatePTPDelayMechanism(p.DelayMechanism); err != nil {
		return err
	}
	if p.Profile == "power" && p.DelayMechanism == "e2e" {
		return fmt.Errorf("power profile requires p2p delay_mechanism")
	}
	if p.PowerProfile.Version != "2011" && p.PowerProfile.Version != "2017" {
		return fmt.Errorf("power_profile.version must be 2011|2017, got %q", p.PowerProfile.Version)
	}
	return nil
}

func validatePTPDelayMechanism(mechanism string) error {
	switch mechanism {
	case "", "e2e", "p2p":
		return nil
	default:
		return fmt.Errorf("delay_mechanism must be e2e|p2p, got %q", mechanism)
	}
}

func (s *SVSubCfg) validate() error {
	if s.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if len(s.Subscriptions) == 0 {
		return fmt.Errorf("at least one subscription is required")
	}
	seen := make(map[string]bool, len(s.Subscriptions))
	for i := range s.Subscriptions {
		sub := &s.Subscriptions[i]
		if sub.Name == "" {
			return fmt.Errorf("subscription #%d: name is required", i)
		}
		if seen[sub.Name] {
			return fmt.Errorf("subscription %q: duplicate name", sub.Name)
		}
		seen[sub.Name] = true
		if sub.DstMAC == "" {
			return fmt.Errorf("subscription %q: dst_mac is required", sub.Name)
		}
		dstMAC, err := parseMAC48(sub.DstMAC)
		if err != nil {
			return fmt.Errorf("subscription %q: invalid dst_mac %q: %w", sub.Name, sub.DstMAC, err)
		}
		if !sub.AllowNonStandard && !sv.IsValidMulticastMAC(dstMAC) {
			return fmt.Errorf("subscription %q: dst_mac %q is outside the IEC 61850 SV multicast range", sub.Name, sub.DstMAC)
		}
		if sub.SrcMAC != "" {
			if _, err := parseMAC48(sub.SrcMAC); err != nil {
				return fmt.Errorf("subscription %q: invalid src_mac %q: %w", sub.Name, sub.SrcMAC, err)
			}
		}
		if !sub.MatchAnyAppID && !sub.AllowNonStandard && !sv.IsValidAppID(uint16(sub.AppID)) {
			return fmt.Errorf("subscription %q: app_id 0x%04X is outside the IEC 61850 SV range", sub.Name, uint16(sub.AppID))
		}
		if sub.VLANID != nil && *sub.VLANID > 4094 {
			return fmt.Errorf("subscription %q: vlan_id must be 0..4094", sub.Name)
		}
		if !isSVBaseVector(sub.BaseVector) {
			return fmt.Errorf("subscription %q: base_vector must be Ia|Ib|Ic|In|Ua|Ub|Uc|Un, got %q", sub.Name, sub.BaseVector)
		}
		switch sub.SmpRate {
		case 80, 256:
		default:
			return fmt.Errorf("subscription %q: smp_rate must be 80|256, got %d", sub.Name, sub.SmpRate)
		}
		switch sub.SampleTimingFrequency {
		case 50, 60:
		default:
			return fmt.Errorf("subscription %q: sample_timing_frequency must be 50|60, got %d", sub.Name, sub.SampleTimingFrequency)
		}
		if math.IsNaN(sub.CurrentCoefficient) || math.IsInf(sub.CurrentCoefficient, 0) || sub.CurrentCoefficient <= 0 {
			return fmt.Errorf("subscription %q: current_coefficient must be finite and greater than 0", sub.Name)
		}
		if math.IsNaN(sub.VoltageCoefficient) || math.IsInf(sub.VoltageCoefficient, 0) || sub.VoltageCoefficient <= 0 {
			return fmt.Errorf("subscription %q: voltage_coefficient must be finite and greater than 0", sub.Name)
		}
	}
	return nil
}

// GoosePubCfg configures the GOOSE publication module (IEC 61850-8-1).
type GoosePubCfg struct {
	Enabled    bool             `yaml:"enabled"`
	Interface  string           `yaml:"interface"`  // network interface for injection (required)
	Publishers []GoosePublisher `yaml:"publishers"` // publisher list
}

// GoosePublisher defines one GOOSE publisher.
type GoosePublisher struct {
	Name             string              `yaml:"name"`
	DstMAC           string              `yaml:"dst_mac"`
	SrcMAC           string              `yaml:"src_mac"`
	AppID            HexU16              `yaml:"app_id"`
	AllowNonStandard bool                `yaml:"allow_nonstandard"`
	GocbRef          string              `yaml:"gocb_ref"`
	DatSet           string              `yaml:"dat_set"`
	GoID             string              `yaml:"go_id"`
	ConfRev          uint32              `yaml:"conf_rev"`
	NdsCom           bool                `yaml:"nds_com"`
	VLANID           *uint16             `yaml:"vlan_id"`
	VLANPri          *uint8              `yaml:"vlan_pri"`        // PCP 0..7
	MinIntervalMs    uint32              `yaml:"min_interval_ms"` // rapid retransmissions after a state change
	MaxIntervalMs    uint32              `yaml:"max_interval_ms"` // heartbeat / timeAllowedToLive
	Dataset          []GooseDatasetEntry `yaml:"dataset"`         // allData order
}

// GooseDatasetEntry defines one allData element in a GOOSE DataSet.
// Name is used in the GUI/project; only the type and value are transmitted in a GOOSE frame.
type GooseDatasetEntry struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // bool, quality, int, uint, float, string, utc_time
}

func (g *GoosePubCfg) validate() error {
	if g.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if len(g.Publishers) == 0 {
		return fmt.Errorf("at least one publisher is required")
	}
	seen := make(map[string]bool, len(g.Publishers))
	for i := range g.Publishers {
		pub := &g.Publishers[i]
		if pub.Name == "" {
			return fmt.Errorf("publisher #%d: name is required", i)
		}
		if seen[pub.Name] {
			return fmt.Errorf("publisher %q: duplicate name", pub.Name)
		}
		seen[pub.Name] = true
		if pub.DstMAC == "" {
			return fmt.Errorf("publisher %q: dst_mac is required", pub.Name)
		}
		dstMAC, err := parseMAC48(pub.DstMAC)
		if err != nil {
			return fmt.Errorf("publisher %q: invalid dst_mac %q: %w", pub.Name, pub.DstMAC, err)
		}
		if !pub.AllowNonStandard && !goose.IsValidMulticastMAC(dstMAC) {
			return fmt.Errorf("publisher %q: dst_mac %q is outside the IEC 61850 GOOSE multicast range", pub.Name, pub.DstMAC)
		}
		if pub.SrcMAC == "" {
			return fmt.Errorf("publisher %q: src_mac is required", pub.Name)
		}
		if _, err := parseMAC48(pub.SrcMAC); err != nil {
			return fmt.Errorf("publisher %q: invalid src_mac %q: %w", pub.Name, pub.SrcMAC, err)
		}
		if !pub.AllowNonStandard && !goose.IsValidAppID(uint16(pub.AppID)) {
			return fmt.Errorf("publisher %q: app_id 0x%04X is outside the IEC 61850 GOOSE range", pub.Name, uint16(pub.AppID))
		}
		if pub.GocbRef == "" {
			return fmt.Errorf("publisher %q: gocb_ref is required", pub.Name)
		}
		if pub.MinIntervalMs == 0 {
			return fmt.Errorf("publisher %q: min_interval_ms must be > 0", pub.Name)
		}
		if pub.MaxIntervalMs == 0 {
			return fmt.Errorf("publisher %q: max_interval_ms must be > 0", pub.Name)
		}
		if pub.MinIntervalMs > pub.MaxIntervalMs {
			return fmt.Errorf("publisher %q: min_interval_ms must be <= max_interval_ms", pub.Name)
		}
		if pub.VLANID != nil && *pub.VLANID > 4094 {
			return fmt.Errorf("publisher %q: vlan_id must be 0..4094", pub.Name)
		}
		if pub.VLANPri != nil && *pub.VLANPri > 7 {
			return fmt.Errorf("publisher %q: vlan_pri must be 0..7", pub.Name)
		}
		if pub.VLANPri != nil && pub.VLANID == nil {
			return fmt.Errorf("publisher %q: vlan_id is required when vlan_pri is set", pub.Name)
		}
		if len(pub.Dataset) == 0 {
			return fmt.Errorf("publisher %q: dataset is required", pub.Name)
		}
		for j, entry := range pub.Dataset {
			if entry.Name == "" {
				return fmt.Errorf("publisher %q: dataset item #%d name is required", pub.Name, j)
			}
			if !isGooseDatasetType(entry.Type) {
				return fmt.Errorf("publisher %q: dataset item %q has unsupported type %q", pub.Name, entry.Name, entry.Type)
			}
		}
	}
	return nil
}

func isGooseDatasetType(t string) bool {
	switch t {
	case "bool", "quality", "int", "uint", "float", "string", "utc_time":
		return true
	default:
		return false
	}
}

// SVPubCfg configures the SV publication module (IEC 61850-9-2LE).
type SVPubCfg struct {
	Enabled   bool          `yaml:"enabled"`
	Interface string        `yaml:"interface"` // network interface (required)
	Streams   []SVPublisher `yaml:"streams"`   // SV stream list
}

// SVPublisher defines one SV stream.
type SVPublisher struct {
	Name                  string  `yaml:"name"`
	DstMAC                string  `yaml:"dst_mac"`
	SrcMAC                string  `yaml:"src_mac"`
	AppID                 HexU16  `yaml:"app_id"`
	AllowNonStandard      bool    `yaml:"allow_nonstandard"`
	SvID                  string  `yaml:"sv_id"`
	DatSet                string  `yaml:"dat_set"` // DataSet name for SCL/ICD; not a 9-2LE Publisher wire field
	ConfRev               uint32  `yaml:"conf_rev"`
	VLANID                *uint16 `yaml:"vlan_id"`
	VLANPri               *uint8  `yaml:"vlan_pri"`                // PCP 0..7
	SmpRate               uint16  `yaml:"smp_rate"`                // samples per period (80 or 256)
	SampleTimingFrequency uint16  `yaml:"sample_timing_frequency"` // SV frame timing frequency in Hz (50 or 60)
	SmpSynch              uint8   `yaml:"smp_synch"`               // 0=none, 1=local, 2=global
}

func (s *SVPubCfg) validate() error {
	if s.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if len(s.Streams) == 0 {
		return fmt.Errorf("at least one stream is required")
	}
	seen := make(map[string]bool, len(s.Streams))
	for i := range s.Streams {
		st := &s.Streams[i]
		if st.Name == "" {
			return fmt.Errorf("stream #%d: name is required", i)
		}
		if seen[st.Name] {
			return fmt.Errorf("stream %q: duplicate name", st.Name)
		}
		seen[st.Name] = true
		if st.DstMAC == "" {
			return fmt.Errorf("stream %q: dst_mac is required", st.Name)
		}
		dstMAC, err := parseMAC48(st.DstMAC)
		if err != nil {
			return fmt.Errorf("stream %q: invalid dst_mac %q: %w", st.Name, st.DstMAC, err)
		}
		if !st.AllowNonStandard && !sv.IsValidMulticastMAC(dstMAC) {
			return fmt.Errorf("stream %q: dst_mac %q is outside the IEC 61850 SV multicast range", st.Name, st.DstMAC)
		}
		if st.SrcMAC == "" {
			return fmt.Errorf("stream %q: src_mac is required", st.Name)
		}
		if _, err := parseMAC48(st.SrcMAC); err != nil {
			return fmt.Errorf("stream %q: invalid src_mac %q: %w", st.Name, st.SrcMAC, err)
		}
		if !st.AllowNonStandard && !sv.IsValidAppID(uint16(st.AppID)) {
			return fmt.Errorf("stream %q: app_id 0x%04X is outside the IEC 61850 SV range", st.Name, uint16(st.AppID))
		}
		if st.SvID == "" {
			return fmt.Errorf("stream %q: sv_id is required", st.Name)
		}
		if st.SmpRate != 80 {
			return fmt.Errorf("stream %q: 9-2LE Publisher supports smp_rate 80, got %d", st.Name, st.SmpRate)
		}
		switch st.SampleTimingFrequency {
		case 50, 60:
		default:
			return fmt.Errorf("stream %q: sample_timing_frequency must be 50|60, got %d", st.Name, st.SampleTimingFrequency)
		}
		if st.VLANPri != nil && *st.VLANPri > 7 {
			return fmt.Errorf("stream %q: vlan_pri must be 0..7", st.Name)
		}
		if st.VLANID != nil && *st.VLANID > 4094 {
			return fmt.Errorf("stream %q: vlan_id must be 0..4094", st.Name)
		}
		if st.VLANPri != nil && st.VLANID == nil {
			return fmt.Errorf("stream %q: vlan_id is required when vlan_pri is set", st.Name)
		}
		if st.SmpSynch > 2 {
			return fmt.Errorf("stream %q: smp_synch must be 0..2", st.Name)
		}
	}
	return nil
}

func isSVBaseVector(v string) bool {
	switch v {
	case "Ia", "Ib", "Ic", "In", "Ua", "Ub", "Uc", "Un":
		return true
	default:
		return false
	}
}

func parseMAC48(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(value)
	if err != nil {
		return nil, err
	}
	if len(mac) != 6 {
		return nil, fmt.Errorf("must be a 48-bit MAC address")
	}
	return mac, nil
}

// PTPServerCfg configures the PTP server module (IEEE 1588-2008 grandmaster).
type PTPServerCfg struct {
	Enabled   bool   `yaml:"enabled"`
	Interface string `yaml:"interface"` // network interface (required)
	Profile   string `yaml:"profile"`   // "default" or "power" (IEEE C37.238)
	Transport string `yaml:"transport"` // "udp" or "ethernet"
	// DeprecatedTimestampMode only lets projects created by older releases
	// load successfully. Its value is discarded by applyDefaults.
	DeprecatedTimestampMode string             `yaml:"timestamp_mode,omitempty"`
	DomainNumber            *uint8             `yaml:"domain_number"`              // nil uses the profile value
	DelayMechanism          string             `yaml:"delay_mechanism"`            // "e2e" or "p2p"; omitted uses the profile value
	UTCOffset               *int16             `yaml:"utc_offset"`                 // TAI-UTC offset (37 by default)
	TimeSource              string             `yaml:"time_source"`                // "gps", "ptp", "ntp", "internal_oscillator", etc.
	Priority1               *uint8             `yaml:"priority1"`                  // nil uses the profile value (128)
	Priority2               *uint8             `yaml:"priority2"`                  // nil uses the profile value (128)
	ClockClass              *uint8             `yaml:"clock_class"`                // nil uses the profile value
	ClockAccuracy           *uint8             `yaml:"clock_accuracy"`             // nil uses the profile value
	ClockVariance           *uint16            `yaml:"offset_scaled_log_variance"` // nil uses the profile value
	TimeTraceable           *bool              `yaml:"time_traceable"`             // nil is false; true only for a verified source
	FrequencyTraceable      *bool              `yaml:"frequency_traceable"`        // nil is false; true only for a verified source
	PowerProfile            PTPPowerProfileCfg `yaml:"power_profile"`              // IEEE C37.238 wire-version parameters
}

// PTPPowerProfileCfg defines the Power Profile Organization Extension TLV fields.
// Separate 2011 and 2017 inaccuracy fields reflect the standard's different wire formats.
type PTPPowerProfileCfg struct {
	Version                   string                    `yaml:"version"`
	GrandmasterID             *uint16                   `yaml:"grandmaster_id"`
	GrandmasterTimeInaccuracy *uint32                   `yaml:"grandmaster_time_inaccuracy"`
	NetworkTimeInaccuracy     *uint32                   `yaml:"network_time_inaccuracy"`
	TotalTimeInaccuracy       *uint32                   `yaml:"total_time_inaccuracy"`
	AlternateTimeOffset       PTPAlternateTimeOffsetCfg `yaml:"alternate_time_offset"`
}

// PTPAlternateTimeOffsetCfg defines an ALTERNATE_TIME_OFFSET_INDICATOR TLV,
// which may follow either version of the C37.238 profile TLV.
type PTPAlternateTimeOffsetCfg struct {
	Enabled        *bool   `yaml:"enabled"`
	KeyField       *uint8  `yaml:"key_field"`
	CurrentOffset  *int32  `yaml:"current_offset"`
	JumpSeconds    *int32  `yaml:"jump_seconds"`
	TimeOfNextJump *uint64 `yaml:"time_of_next_jump"`
	DisplayName    string  `yaml:"display_name"`
}

func applyPTPPowerProfileDefaults(p *PTPPowerProfileCfg) {
	if p.Version == "" {
		p.Version = "2011"
	}
	if p.GrandmasterID == nil {
		value := uint16(3)
		p.GrandmasterID = &value
	}
	if p.GrandmasterTimeInaccuracy == nil {
		value := uint32(60)
		p.GrandmasterTimeInaccuracy = &value
	}
	if p.NetworkTimeInaccuracy == nil {
		value := uint32(0)
		p.NetworkTimeInaccuracy = &value
	}
	if p.TotalTimeInaccuracy == nil {
		value := uint32(100)
		p.TotalTimeInaccuracy = &value
	}
	if p.AlternateTimeOffset.Enabled == nil {
		value := true
		p.AlternateTimeOffset.Enabled = &value
	}
	if p.AlternateTimeOffset.KeyField == nil {
		value := uint8(1)
		p.AlternateTimeOffset.KeyField = &value
	}
	if p.AlternateTimeOffset.CurrentOffset == nil {
		value := int32(10763)
		p.AlternateTimeOffset.CurrentOffset = &value
	}
	if p.AlternateTimeOffset.JumpSeconds == nil {
		value := int32(0)
		p.AlternateTimeOffset.JumpSeconds = &value
	}
	if p.AlternateTimeOffset.TimeOfNextJump == nil {
		value := uint64(0)
		p.AlternateTimeOffset.TimeOfNextJump = &value
	}
	if p.AlternateTimeOffset.DisplayName == "" {
		p.AlternateTimeOffset.DisplayName = "UTC+03:00"
	}
}

func (p *PTPServerCfg) validate() error {
	if p.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	switch p.Profile {
	case "", "default", "power":
	default:
		return fmt.Errorf("profile must be default|power, got %q", p.Profile)
	}
	switch p.Transport {
	case "", "udp", "ethernet":
	default:
		return fmt.Errorf("transport must be udp|ethernet, got %q", p.Transport)
	}
	if p.Profile == "power" && p.Transport != "ethernet" {
		return fmt.Errorf("power profile requires ethernet transport")
	}
	if err := validatePTPDelayMechanism(p.DelayMechanism); err != nil {
		return err
	}
	if p.Profile == "power" && p.DelayMechanism == "e2e" {
		return fmt.Errorf("power profile requires p2p delay_mechanism")
	}
	if p.TimeSource != "" {
		if _, ok := ParseTimeSource(p.TimeSource); !ok {
			return fmt.Errorf("unknown time_source %q", p.TimeSource)
		}
	}
	if p.PowerProfile.Version != "2011" && p.PowerProfile.Version != "2017" {
		return fmt.Errorf("power_profile.version must be 2011|2017, got %q", p.PowerProfile.Version)
	}
	if len([]byte(p.PowerProfile.AlternateTimeOffset.DisplayName)) > 255 {
		return fmt.Errorf("power_profile.alternate_time_offset.display_name must be at most 255 bytes")
	}
	if value := p.PowerProfile.AlternateTimeOffset.TimeOfNextJump; value != nil && *value > (1<<48)-1 {
		return fmt.Errorf("power_profile.alternate_time_offset.time_of_next_jump must fit in 48 bits")
	}
	return nil
}

// ParseTimeSource converts a time-source name to an IEEE 1588 constant.
func ParseTimeSource(s string) (uint8, bool) {
	switch strings.ToLower(s) {
	case "atomic_clock":
		return 0x10, true
	case "gps":
		return 0x20, true
	case "terrestrial_radio":
		return 0x30, true
	case "serial_time_code":
		return 0x39, true
	case "ptp":
		return 0x40, true
	case "ntp":
		return 0x50, true
	case "hand_set":
		return 0x60, true
	case "other":
		return 0x90, true
	case "internal_oscillator", "":
		return 0xA0, true
	default:
		return 0, false
	}
}
