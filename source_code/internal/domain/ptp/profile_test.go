package ptp

import "testing"

func TestDefaultProfileUsesInteroperableGrandmasterClockQuality(t *testing.T) {
	if DefaultProfile.ClockClass != ClockClass6 ||
		DefaultProfile.ClockAccuracy != ClockAccuracy100ns ||
		DefaultProfile.ClockVariance != ClockVarianceGPS ||
		DefaultProfile.TimeSource != TimeSourceInternalOscillator {
		t.Fatalf("unexpected Default Profile clock quality: %+v", DefaultProfile)
	}
	if DefaultProfile.FlagField&(FlagTimeTraceable|FlagFrequencyTraceable) != 0 {
		t.Fatalf("unexpected Default Profile traceability flags: 0x%04x", DefaultProfile.FlagField)
	}
}

func TestPowerProfileUsesGrandmasterClockQuality(t *testing.T) {
	if PowerProfile.ClockClass != ClockClass6 ||
		PowerProfile.ClockAccuracy != ClockAccuracy100ns ||
		PowerProfile.ClockVariance != ClockVarianceGPS ||
		PowerProfile.TimeSource != TimeSourceGPS {
		t.Fatalf("unexpected Power Profile clock quality: %+v", PowerProfile)
	}
	wantFlags := FlagTimeTraceable | FlagFrequencyTraceable
	if PowerProfile.FlagField&wantFlags != wantFlags {
		t.Fatalf("Power Profile traceability flags are missing: 0x%04x", PowerProfile.FlagField)
	}
}

func TestPowerProfileMessageIntervals(t *testing.T) {
	if PowerProfile.DomainNumber != 0 {
		t.Fatalf("domainNumber: want 0 for C37.238-2011 preset, got %d", PowerProfile.DomainNumber)
	}
	if PowerProfile.C37238Version != C37238Version2011 {
		t.Fatalf("C37.238 version: want 2011, got %s", PowerProfile.C37238Version)
	}
	if PowerProfile.TransportSpecific != 0 {
		t.Fatalf("transportSpecific: want 0 for C37.238, got %d", PowerProfile.TransportSpecific)
	}
	if PowerProfile.LogSyncInterval != 0 {
		t.Fatalf("logSyncInterval: want 0, got %d", PowerProfile.LogSyncInterval)
	}
	if PowerProfile.LogAnnounceInterval != 1 {
		t.Fatalf("logAnnounceInterval: want 1, got %d", PowerProfile.LogAnnounceInterval)
	}
	if PowerProfile.LogPDelayReqInterval != 0 {
		t.Fatalf("logPdelayReqInterval: want 0, got %d", PowerProfile.LogPDelayReqInterval)
	}
}
