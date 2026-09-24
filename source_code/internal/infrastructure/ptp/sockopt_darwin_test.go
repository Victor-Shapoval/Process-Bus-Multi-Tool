//go:build darwin

package ptp

import (
	"errors"
	"testing"
)

func TestDarwinUDPSoftwareTimestampingFailsAtConstruction(t *testing.T) {
	_, err := enableUDPKernelTimestamps(nil, "en0", "software", timestampClockRealtime)
	if !errors.Is(err, ErrSoftwareTimestampingUnavailable) {
		t.Fatalf("expected unavailable strict UDP software timestamping, got %v", err)
	}
}

func TestDarwinGrandmasterRejectsSoftwareFallback(t *testing.T) {
	_, err := NewGrandmaster("en0", "ethernet")
	if !errors.Is(err, ErrHardwareTimestampingUnavailable) {
		t.Fatalf("expected unavailable hardware Grandmaster, got %v", err)
	}
}

func TestDarwinSocketTimestampHelpersNeverFallback(t *testing.T) {
	if _, err := extractKernelTimestamp(nil, "software", nil); !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing RX software timestamp, got %v", err)
	}
	if _, err := readTXTimestampConn(nil, "software", nil); !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing TX software timestamp, got %v", err)
	}
}
