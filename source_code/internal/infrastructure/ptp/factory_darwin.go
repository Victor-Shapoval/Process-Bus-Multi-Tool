//go:build darwin

package ptp

import (
	"fmt"

	"pbmt/internal/application/ptpport"
)

// New creates a PTP transport on macOS. Hardware timestamping is unavailable,
// so auto selects software timestamping.
func New(ifaceName, transportType, timestampMode string) (ptpport.Transport, error) {
	if timestampMode == "" || timestampMode == "auto" {
		timestampMode = "software"
	}
	switch transportType {
	case "ethernet":
		return NewEthernet(ifaceName, timestampMode)
	default:
		return NewUDP(ifaceName, timestampMode)
	}
}

// NewGrandmaster is unavailable on macOS because the platform does not expose
// the PHC hardware timestamps required by the Grandmaster implementation.
func NewGrandmaster(ifaceName, _ string) (ptpport.Transport, error) {
	return nil, fmt.Errorf("%w for PTP Grandmaster on macOS interface %s; software fallback is disabled", ErrHardwareTimestampingUnavailable, ifaceName)
}
