//go:build darwin

package ptp

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

const oobSize = 128

// macOS does not expose the Linux PHC/SO_TIMESTAMPING TX API used by this
// package. Strict UDP software mode therefore cannot provide an event TX
// timestamp and is rejected at construction time. Ethernet software mode uses
// BPF timestamps in ethernet_darwin.go.
type hardwareClockConverter struct{}

func (c *hardwareClockConverter) Close() {}

func (c *hardwareClockConverter) Now() (time.Time, error) { return time.Now().UTC(), nil }

func enableUDPKernelTimestamps(_ *net.UDPConn, ifaceName, mode string, _ timestampClockMode) (*hardwareClockConverter, error) {
	switch mode {
	case "hardware":
		return nil, fmt.Errorf("%w on macOS interface %s", ErrHardwareTimestampingUnavailable, ifaceName)
	case "software":
		return nil, fmt.Errorf("%w for UDP TX on macOS interface %s; use ethernet transport", ErrSoftwareTimestampingUnavailable, ifaceName)
	default:
		return nil, fmt.Errorf("unsupported timestamp mode %q", mode)
	}
}

func extractKernelTimestamp(_ []byte, mode string, _ *hardwareClockConverter) (time.Time, error) {
	return time.Time{}, timestampMissingError(mode, "RX")
}

func drainTXErrQueueConn(_ syscall.RawConn) {}

func txTimestampControlMessage(_ string) []byte { return nil }

func readTXTimestampConn(_ syscall.RawConn, mode string, _ *hardwareClockConverter) (time.Time, error) {
	return time.Time{}, timestampMissingError(mode, "TX")
}
