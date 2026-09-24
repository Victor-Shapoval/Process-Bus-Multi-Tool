//go:build linux

package ptp

import (
	"errors"

	"pbmt/internal/application/ptpport"
)

// New creates a PTP transport of the specified type.
// transportType is "udp" or "ethernet".
// timestampMode is "auto" (default), "software", or "hardware".
func New(ifaceName, transportType, timestampMode string) (ptpport.Transport, error) {
	return newWithClock(ifaceName, transportType, timestampMode, timestampClockRealtime)
}

// NewGrandmaster creates a transport whose hardware timestamps use an
// autonomous monotonic clock initialized from host UTC at startup. A
// Grandmaster is hardware-only and never falls back to software timestamps.
func NewGrandmaster(ifaceName, transportType string) (ptpport.Transport, error) {
	return newTransport(ifaceName, transportType, "hardware", timestampClockAutonomous)
}

func newWithClock(ifaceName, transportType, timestampMode string, clockMode timestampClockMode) (ptpport.Transport, error) {
	if timestampMode == "" || timestampMode == "auto" {
		tr, hardwareErr := newTransport(ifaceName, transportType, "hardware", clockMode)
		if hardwareErr == nil {
			return tr, nil
		}
		tr, softwareErr := newTransport(ifaceName, transportType, "software", timestampClockRealtime)
		if softwareErr == nil {
			return tr, nil
		}
		return nil, errors.Join(hardwareErr, softwareErr)
	}
	return newTransport(ifaceName, transportType, timestampMode, clockMode)
}

func newTransport(ifaceName, transportType, timestampMode string, clockMode timestampClockMode) (ptpport.Transport, error) {
	switch transportType {
	case "ethernet":
		return newEthernet(ifaceName, timestampMode, clockMode)
	default:
		return newUDP(ifaceName, timestampMode, clockMode)
	}
}
