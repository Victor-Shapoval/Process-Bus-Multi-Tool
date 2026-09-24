package config

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultProfileLoads(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	profileDir := filepath.Join(filepath.Dir(file), "..", "..", "profiles", "default")
	cfg, err := Load(profileDir)
	if err != nil {
		t.Fatalf("load default profile: %v", err)
	}
	if len(cfg.GooseSub.Subscriptions) == 0 {
		t.Fatal("default profile has no GOOSE subscriptions")
	}
	sub := cfg.GooseSub.Subscriptions[0]
	if sub.DatSet == "" || sub.ConfRev == nil {
		t.Fatalf("default GOOSE subscriber must define dataset and conf_rev: %+v", sub)
	}
	if sub.VLANID == nil || *sub.VLANID != 1 {
		t.Fatalf("default GOOSE subscriber VLAN ID: got %v, want 1", sub.VLANID)
	}
	if sub.VLANPri == nil || *sub.VLANPri != 4 {
		t.Fatalf("default GOOSE subscriber VLAN priority: got %v, want 4", sub.VLANPri)
	}
	if cfg.PTPServer.TimeSource != "gps" {
		t.Fatalf("default PTP time source: want gps, got %q", cfg.PTPServer.TimeSource)
	}
	if cfg.PTPServer.TimeTraceable == nil || *cfg.PTPServer.TimeTraceable || cfg.PTPServer.FrequencyTraceable == nil || *cfg.PTPServer.FrequencyTraceable {
		t.Fatalf("default PTP traceability must be explicitly disabled: %+v", cfg.PTPServer)
	}
	if cfg.PTPClient.Transport != "ethernet" || cfg.PTPServer.Transport != "ethernet" {
		t.Fatalf("default PTP transports must be ethernet: client=%q server=%q", cfg.PTPClient.Transport, cfg.PTPServer.Transport)
	}
	if cfg.PTPClient.DelayMechanism != "e2e" || cfg.PTPServer.DelayMechanism != "e2e" {
		t.Fatalf("default PTP delay mechanisms must be e2e: client=%q server=%q", cfg.PTPClient.DelayMechanism, cfg.PTPServer.DelayMechanism)
	}
	if cfg.PTPClient.PowerProfile.Version != "2011" {
		t.Fatalf("default PTP Client C37.238 version: want 2011, got %q", cfg.PTPClient.PowerProfile.Version)
	}
	if cfg.PTPClient.DomainNumber == nil || *cfg.PTPClient.DomainNumber != 0 ||
		cfg.PTPServer.DomainNumber == nil || *cfg.PTPServer.DomainNumber != 0 {
		t.Fatalf("default PTP domain numbers must be explicitly zero: client=%v server=%v", cfg.PTPClient.DomainNumber, cfg.PTPServer.DomainNumber)
	}
	power := cfg.PTPServer.PowerProfile
	if power.Version != "2011" || power.GrandmasterID == nil || *power.GrandmasterID != 3 ||
		power.GrandmasterTimeInaccuracy == nil || *power.GrandmasterTimeInaccuracy != 60 ||
		power.NetworkTimeInaccuracy == nil || *power.NetworkTimeInaccuracy != 0 ||
		power.AlternateTimeOffset.Enabled == nil || !*power.AlternateTimeOffset.Enabled ||
		power.AlternateTimeOffset.DisplayName != "UTC+03:00" {
		t.Fatalf("default C37.238-2011 settings do not match the interoperable preset: %+v", power)
	}
}

func TestPTPClientDefaultsAndPowerTransportValidation(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.PTPClient.Source != "external" {
		t.Fatalf("missing source must retain External mode: %q", cfg.PTPClient.Source)
	}
	if cfg.PTPClient.Profile != "default" || cfg.PTPClient.Transport != "udp" || cfg.PTPClient.DelayMechanism != "e2e" {
		t.Fatalf("PTP Client defaults: %+v", cfg.PTPClient)
	}
	client := PTPClientCfg{Interface: "en0", Profile: "power", Transport: "udp"}
	if err := client.validate(); err == nil || !strings.Contains(err.Error(), "requires ethernet") {
		t.Fatalf("expected Power Profile transport error, got %v", err)
	}
}

func TestPTPClientSourceValidation(t *testing.T) {
	local := PTPClientCfg{Source: "local"}
	if err := local.validate(); err != nil {
		t.Fatalf("Local must not require network settings: %v", err)
	}
	for _, source := range []string{"auto", "Local", "unknown"} {
		if err := (&PTPClientCfg{Source: source}).validate(); err == nil {
			t.Fatalf("invalid source %q accepted", source)
		}
	}
	if err := (&PTPClientCfg{Source: "external"}).validate(); err == nil {
		t.Fatal("External accepted without interface")
	}
}

func TestPTPServerDefaultsAndPowerTransportValidation(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.PTPServer.Profile != "default" || cfg.PTPServer.Transport != "udp" || cfg.PTPServer.DelayMechanism != "e2e" {
		t.Fatalf("PTP defaults: %+v", cfg.PTPServer)
	}
	if cfg.PTPServer.UTCOffset == nil || *cfg.PTPServer.UTCOffset != 37 {
		t.Fatalf("PTP UTC offset default: %v", cfg.PTPServer.UTCOffset)
	}
	if cfg.PTPServer.PowerProfile.Version != "2011" {
		t.Fatalf("C37.238 version default: want 2011, got %q", cfg.PTPServer.PowerProfile.Version)
	}
	server := PTPServerCfg{Interface: "en0", Profile: "power", Transport: "udp"}
	if err := server.validate(); err == nil || !strings.Contains(err.Error(), "requires ethernet") {
		t.Fatalf("expected Power Profile transport error, got %v", err)
	}
}

func TestLegacyPTPTimestampModeIsAcceptedAndIgnored(t *testing.T) {
	for _, file := range []string{"ptp_client.yaml", "ptp_server.yaml"} {
		for _, mode := range []string{"auto", "software", "hardware"} {
			t.Run(file+"/"+mode, func(t *testing.T) {
				profileDir := copyDefaultProfile(t)
				candidate := []byte("enabled: false\ntimestamp_mode: " + mode + "\n")
				cfg, err := SaveProfileFile(profileDir, file, candidate)
				if err != nil {
					t.Fatalf("load legacy timestamp_mode %q: %v", mode, err)
				}
				if cfg.PTPClient.DeprecatedTimestampMode != "" || cfg.PTPServer.DeprecatedTimestampMode != "" {
					t.Fatalf("legacy timestamp_mode leaked into active config: client=%q server=%q",
						cfg.PTPClient.DeprecatedTimestampMode, cfg.PTPServer.DeprecatedTimestampMode)
				}
			})
		}
	}
}

func TestPTPPowerProfileVersionValidation(t *testing.T) {
	cfg := Config{PTPServer: PTPServerCfg{
		Interface: "en0",
		Profile:   "power",
		Transport: "ethernet",
		PowerProfile: PTPPowerProfileCfg{
			Version: "2008",
		},
	}}
	cfg.applyDefaults()
	if err := cfg.PTPServer.validate(); err == nil || !strings.Contains(err.Error(), "power_profile.version") {
		t.Fatalf("expected C37.238 version validation error, got %v", err)
	}
}

func TestPTPClientPowerProfileVersionValidation(t *testing.T) {
	client := PTPClientCfg{
		Interface:      "en0",
		Profile:        "power",
		Transport:      "ethernet",
		DelayMechanism: "p2p",
		PowerProfile:   PTPClientPowerProfileCfg{Version: "2008"},
	}
	if err := client.validate(); err == nil || !strings.Contains(err.Error(), "power_profile.version") {
		t.Fatalf("expected client C37.238 version validation error, got %v", err)
	}
}

func TestPTPServerTraceabilityRequiresExplicitOptIn(t *testing.T) {
	for _, source := range []string{"gps", "ptp", "internal_oscillator", ""} {
		t.Run(source, func(t *testing.T) {
			cfg := Config{PTPServer: PTPServerCfg{Profile: "power", TimeSource: source}}
			cfg.applyDefaults()
			if cfg.PTPServer.TimeTraceable == nil || cfg.PTPServer.FrequencyTraceable == nil {
				t.Fatal("traceability defaults were not materialized")
			}
			if *cfg.PTPServer.TimeTraceable || *cfg.PTPServer.FrequencyTraceable {
				t.Fatalf("source %q enabled traceability without explicit opt-in", source)
			}
		})
	}

	enabled := true
	cfg := Config{PTPServer: PTPServerCfg{
		Profile:            "power",
		TimeSource:         "gps",
		TimeTraceable:      &enabled,
		FrequencyTraceable: &enabled,
	}}
	cfg.applyDefaults()
	if !*cfg.PTPServer.TimeTraceable || !*cfg.PTPServer.FrequencyTraceable {
		t.Fatal("explicit traceability opt-in was overwritten")
	}
}

func TestPTPServerOffsetScaledLogVarianceLoads(t *testing.T) {
	profileDir := copyDefaultProfile(t)
	candidate := []byte(`enabled: false
profile: power
transport: ethernet
offset_scaled_log_variance: 0x4E5D
`)
	cfg, err := SaveProfileFile(profileDir, "ptp_server.yaml", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PTPServer.ClockVariance == nil || *cfg.PTPServer.ClockVariance != 0x4E5D {
		t.Fatalf("offsetScaledLogVariance: want 0x4E5D, got %v", cfg.PTPServer.ClockVariance)
	}
}

func TestPTPDelayMechanismDefaultsFollowProfileAndPowerRequiresP2P(t *testing.T) {
	cfg := Config{
		PTPClient: PTPClientCfg{Profile: "power", DelayMechanism: "e2e"},
		PTPServer: PTPServerCfg{Profile: "power"},
	}
	cfg.applyDefaults()
	if cfg.PTPClient.DelayMechanism != "p2p" {
		t.Fatalf("power-profile client mechanism: want p2p, got %q", cfg.PTPClient.DelayMechanism)
	}
	if cfg.PTPServer.DelayMechanism != "p2p" {
		t.Fatalf("power-profile server mechanism: want p2p, got %q", cfg.PTPServer.DelayMechanism)
	}
	if cfg.PTPClient.Transport != "ethernet" || cfg.PTPServer.Transport != "ethernet" {
		t.Fatalf("Power Profile transports must be Ethernet: client=%q server=%q", cfg.PTPClient.Transport, cfg.PTPServer.Transport)
	}

	for role, validate := range map[string]func() error{
		"client": func() error { return (&PTPClientCfg{Interface: "en0", DelayMechanism: "invalid"}).validate() },
		"server": func() error { return (&PTPServerCfg{Interface: "en0", DelayMechanism: "invalid"}).validate() },
	} {
		if err := validate(); err == nil || !strings.Contains(err.Error(), "delay_mechanism") {
			t.Errorf("%s: expected delay_mechanism validation error, got %v", role, err)
		}
	}

	for role, validate := range map[string]func() error{
		"client": func() error {
			return (&PTPClientCfg{Interface: "en0", Profile: "power", Transport: "ethernet", DelayMechanism: "e2e"}).validate()
		},
		"server": func() error {
			cfg := &PTPServerCfg{Interface: "en0", Profile: "power", Transport: "ethernet", DelayMechanism: "e2e"}
			applyPTPPowerProfileDefaults(&cfg.PowerProfile)
			return cfg.validate()
		},
	} {
		if err := validate(); err == nil || !strings.Contains(err.Error(), "requires p2p") {
			t.Errorf("%s: expected Power Profile P2P validation error, got %v", role, err)
		}
	}
}

func TestLoadRejectsUnknownYAMLFields(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"goose_sub.yaml":  "enabled: false\nunexpected: true\n",
		"goose_pub.yaml":  "enabled: false\n",
		"sv_sub.yaml":     "enabled: false\n",
		"sv_pub.yaml":     "enabled: false\n",
		"ptp_client.yaml": "enabled: false\n",
		"ptp_server.yaml": "enabled: false\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestLoadRequiresOnlyProtocolProfileFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"goose_sub.yaml",
		"goose_pub.yaml",
		"sv_sub.yaml",
		"sv_pub.yaml",
		"ptp_client.yaml",
		"ptp_server.yaml",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("enabled: false\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("load protocol-only profile: %v", err)
	}
}

func TestSVSubscriberTimingFrequencyDefaultsTo50Hz(t *testing.T) {
	cfg := Config{SVSub: SVSubCfg{Subscriptions: []SVSubscriber{{}}}}
	cfg.applyDefaults()
	if got := cfg.SVSub.Subscriptions[0].SampleTimingFrequency; got != 50 {
		t.Fatalf("sample timing frequency: want 50, got %d", got)
	}
}

func TestGoosePublisherRejectsMinIntervalAboveMaxInterval(t *testing.T) {
	cfg := GoosePubCfg{
		Interface: "en0",
		Publishers: []GoosePublisher{{
			Name:          "pub1",
			DstMAC:        "01:0c:cd:01:00:01",
			SrcMAC:        "00:11:22:33:44:55",
			GocbRef:       "IED/LLN0$GO$gcb1",
			MinIntervalMs: 20,
			MaxIntervalMs: 10,
			Dataset:       []GooseDatasetEntry{{Name: "GGIO1.Ind1.stVal", Type: "bool"}},
		}},
	}

	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "min_interval_ms must be <= max_interval_ms") {
		t.Fatalf("expected interval validation error, got %v", err)
	}
}

func TestGooseSubscriberValidatesVLANParameters(t *testing.T) {
	invalidVID := uint16(4095)
	validVID := uint16(100)
	invalidPriority := uint8(8)
	validPriority := uint8(4)
	tests := []struct {
		name     string
		vlanID   *uint16
		vlanPri  *uint8
		wantText string
	}{
		{"reserved VLAN ID", &invalidVID, nil, "vlan_id must be 0..4094"},
		{"invalid priority", &validVID, &invalidPriority, "vlan_pri must be 0..7"},
		{"priority without VLAN", nil, &validPriority, "vlan_id is required when vlan_pri is set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := GooseSubCfg{
				Interface: "en0",
				Subscriptions: []GooseSubscriber{{
					Name:    "sub1",
					DstMAC:  "01:0c:cd:01:00:01",
					VLANID:  tc.vlanID,
					VLANPri: tc.vlanPri,
				}},
			}
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q, got %v", tc.wantText, err)
			}
		})
	}
}

func TestSaveProfileFileRejectsInvalidCandidateWithoutChangingFile(t *testing.T) {
	profileDir := copyDefaultProfile(t)
	path := filepath.Join(profileDir, "goose_sub.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := []byte(`enabled: true
interface: en0
subscriptions:
  - name: bad
    dst_mac: 01:0c:cd:01:00:01:02:03
    app_id: 0x0001
`)
	if _, err := SaveProfileFile(profileDir, "goose_sub.yaml", candidate); err == nil || !strings.Contains(err.Error(), "48-bit") {
		t.Fatalf("expected MAC-48 validation error, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("invalid candidate changed the config file")
	}
	temps, err := filepath.Glob(filepath.Join(profileDir, ".goose_sub.yaml.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", temps)
	}
}

func TestSaveProfileFileCommitsValidatedCandidate(t *testing.T) {
	profileDir := copyDefaultProfile(t)
	candidate := []byte(`enabled: true
interface: en0
subscriptions:
  - name: exact-zero
    dst_mac: 01:0c:cd:01:00:01
    app_id: 0x0000
`)
	cfg, err := SaveProfileFile(profileDir, "goose_sub.yaml", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GooseSub.Subscriptions) != 1 || cfg.GooseSub.Subscriptions[0].AppID != 0 || cfg.GooseSub.Subscriptions[0].MatchAnyAppID {
		t.Fatalf("unexpected committed config: %+v", cfg.GooseSub.Subscriptions)
	}
	written, err := os.ReadFile(filepath.Join(profileDir, "goose_sub.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, candidate) {
		t.Fatalf("written candidate differs: %q", written)
	}
}

func TestProtocolRangesAreStrictByDefaultWithExplicitOptIn(t *testing.T) {
	base := GoosePubCfg{
		Interface: "en0",
		Publishers: []GoosePublisher{{
			Name:          "pub1",
			DstMAC:        "01:0c:cd:04:00:01",
			SrcMAC:        "00:11:22:33:44:55",
			AppID:         0x4001,
			GocbRef:       "IED/LLN0$GO$gcb1",
			MinIntervalMs: 10,
			MaxIntervalMs: 1000,
			Dataset:       []GooseDatasetEntry{{Name: "GGIO1.Ind1.stVal", Type: "bool"}},
		}},
	}
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "GOOSE multicast range") {
		t.Fatalf("expected strict protocol range error, got %v", err)
	}
	base.Publishers[0].AllowNonStandard = true
	if err := base.validate(); err != nil {
		t.Fatalf("explicit non-standard protocol mode was rejected: %v", err)
	}
}

func TestSVVLANValidation(t *testing.T) {
	invalidVID := uint16(4095)
	priority := uint8(4)
	base := SVPublisher{
		Name:                  "sv1",
		DstMAC:                "01:0c:cd:04:00:01",
		SrcMAC:                "00:11:22:33:44:55",
		AppID:                 0x4001,
		SvID:                  "PBMTMU0101",
		SmpRate:               80,
		SampleTimingFrequency: 50,
	}
	tests := []struct {
		name     string
		vlanID   *uint16
		vlanPri  *uint8
		wantText string
	}{
		{name: "reserved VLAN ID", vlanID: &invalidVID, wantText: "vlan_id must be 0..4094"},
		{name: "priority without VLAN", vlanPri: &priority, wantText: "vlan_id is required when vlan_pri is set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := base
			stream.VLANID = tc.vlanID
			stream.VLANPri = tc.vlanPri
			cfg := SVPubCfg{Interface: "en0", Streams: []SVPublisher{stream}}
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("expected %q, got %v", tc.wantText, err)
			}
		})
	}
}

func TestSVPublisherRejectsUnsupported256SampleMode(t *testing.T) {
	cfg := SVPubCfg{
		Interface: "en0",
		Streams: []SVPublisher{{
			Name:                  "sv1",
			DstMAC:                "01:0c:cd:04:00:01",
			SrcMAC:                "00:11:22:33:44:55",
			AppID:                 0x4001,
			SvID:                  "PBMTMU0101",
			SmpRate:               256,
			SampleTimingFrequency: 50,
		}},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "supports smp_rate 80") {
		t.Fatalf("expected unsupported 256-sample Publisher error, got %v", err)
	}
}

func TestSVSubscriberRejectsReservedVLANID(t *testing.T) {
	invalidVID := uint16(4095)
	cfg := SVSubCfg{
		Interface: "en0",
		Subscriptions: []SVSubscriber{{
			Name:                  "sv1",
			DstMAC:                "01:0c:cd:04:00:01",
			AppID:                 0x4001,
			VLANID:                &invalidVID,
			SmpRate:               80,
			SampleTimingFrequency: 50,
			BaseVector:            "Ua",
			CurrentCoefficient:    1,
			VoltageCoefficient:    1,
		}},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "vlan_id must be 0..4094") {
		t.Fatalf("expected VLAN ID validation error, got %v", err)
	}
}

func TestSVSubscriberRejectsNonFiniteCoefficients(t *testing.T) {
	base := SVSubscriber{
		Name:                  "sv1",
		DstMAC:                "01:0c:cd:04:00:01",
		AppID:                 0x4001,
		SmpRate:               80,
		SampleTimingFrequency: 50,
		BaseVector:            "Ua",
		CurrentCoefficient:    1,
		VoltageCoefficient:    1,
	}
	for _, tc := range []struct {
		name  string
		value float64
	}{
		{name: "NaN", value: math.NaN()},
		{name: "+Inf", value: math.Inf(1)},
		{name: "-Inf", value: math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := base
			sub.CurrentCoefficient = tc.value
			cfg := SVSubCfg{Interface: "en0", Subscriptions: []SVSubscriber{sub}}
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "must be finite") {
				t.Fatalf("expected finite coefficient validation error, got %v", err)
			}
		})
	}
}

func copyDefaultProfile(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	source := filepath.Join(filepath.Dir(file), "..", "..", "profiles", "default")
	destination := t.TempDir()
	for _, name := range []string{"goose_sub.yaml", "goose_pub.yaml", "sv_sub.yaml", "sv_pub.yaml", "ptp_client.yaml", "ptp_server.yaml"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}
