// Package ptp provides PTP UDP multicast and L2 Ethernet adapters.
// Linux uses AF_PACKET and kernel timestamping; macOS uses UDP sockets or
// libpcap-based L2 with software timestamps.
package ptp

import (
	"errors"
	"fmt"
	"net"

	ptpdomain "pbmt/internal/domain/ptp"
)

var (
	ErrHardwareTimestampingUnavailable = errors.New("hardware timestamping is unavailable")
	ErrHardwareTimestampMissing        = errors.New("hardware packet timestamp is missing")
	ErrSoftwareTimestampingUnavailable = errors.New("software timestamping is unavailable")
	ErrSoftwareTimestampMissing        = errors.New("software packet timestamp is missing")
)

func timestampMissingError(mode, direction string) error {
	if mode == "hardware" {
		return fmt.Errorf("%w on %s", ErrHardwareTimestampMissing, direction)
	}
	return fmt.Errorf("%w on %s", ErrSoftwareTimestampMissing, direction)
}

type timestampClockMode uint8

const (
	timestampClockRealtime timestampClockMode = iota
	timestampClockAutonomous
)

const (
	recvBufSize = 2000
	chanBufSize = 128
)

// isPDelayEvent reports whether an event message must use the dedicated
// peer-delay multicast destination instead of the primary PTP multicast.
func isPDelayEvent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch data[0] & 0x0F {
	case ptpdomain.MsgPDelayReq, ptpdomain.MsgPDelayResp:
		return true
	default:
		return false
	}
}

// isPDelayGeneral reports whether a general message must use the peer-delay
// multicast destination.
func isPDelayGeneral(data []byte) bool {
	return len(data) != 0 && data[0]&0x0F == ptpdomain.MsgPDelayRespFollowUp
}

func udpGeneralDestination(dst net.Addr) (*net.UDPAddr, error) {
	udpAddr, ok := dst.(*net.UDPAddr)
	if !ok || udpAddr == nil {
		addr := "<nil>"
		if dst != nil && !ok {
			addr = dst.String()
		}
		return nil, &net.AddrError{Err: "expected UDP address", Addr: addr}
	}
	copyAddr := *udpAddr
	copyAddr.IP = append(net.IP(nil), udpAddr.IP...)
	copyAddr.Port = ptpdomain.UDPGeneralPort
	return &copyAddr, nil
}
