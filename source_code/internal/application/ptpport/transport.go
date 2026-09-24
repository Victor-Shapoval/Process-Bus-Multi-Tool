// Package ptpport defines the transport port used by the PTP application
// services. Infrastructure adapters implement this contract.
package ptpport

import (
	"net"
	"time"
)

// Packet is a received PTP packet together with its transport timestamp.
// Event messages always have a non-zero kernel, BPF or hardware timestamp.
// General messages do not participate in delay calculations and may have a
// zero Timestamp.
type Packet struct {
	Data      []byte
	From      net.Addr
	Timestamp time.Time
}

// TimeSource is implemented by transports that own the clock domain used by
// their packet timestamps. Now returns the same pre-currentUtcOffset time scale
// as Packet.Timestamp; the Grandmaster applies its UTC offset exactly once when
// encoding PTP timestamps. This keeps Announce, Sync and delay measurements on
// one time scale.
type TimeSource interface {
	Now() (time.Time, error)
}

// Transport is the network boundary required by the PTP client and server.
type Transport interface {
	Start()
	Close()
	TimestampMode() string
	// SendEvent and SendEventTo return a non-zero transport timestamp whenever
	// err is nil. Implementations must fail instead of substituting time.Now.
	SendEvent(data []byte) (txTime time.Time, err error)
	SendGeneral(data []byte) error
	SendEventTo(data []byte, dst net.Addr) (txTime time.Time, err error)
	SendGeneralTo(data []byte, dst net.Addr) error
	EventCh() <-chan Packet
	GeneralCh() <-chan Packet
	Errors() <-chan error
}
