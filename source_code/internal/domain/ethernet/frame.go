// Package ethernet contains Layer 2 domain types: Ethernet frames, 802.1Q VLAN
// tags, and the Source capture port. It is used by modules that operate on raw
// Ethernet traffic (GOOSE and SV).
package ethernet

import "time"

// EtherTypeVLAN is the 802.1Q EtherType for a VLAN tag.
const EtherTypeVLAN = 0x8100

// VLANTag is an 802.1Q tag.
type VLANTag struct {
	Priority uint8  // PCP, 0..7 (typically 4..7 on the Process Bus)
	DEI      bool   // Drop Eligible Indicator
	VID      uint16 // VLAN ID, 0..4094
}

// Frame is a raw Ethernet frame received from the capture source.
// The caller owns Data only until the next read from Source.
// Data must be copied if the frame needs to be retained.
type Frame struct {
	Data          []byte
	Timestamp     time.Time // capture time (kernel software or NIC hardware)
	ObservedAt    time.Time // application receive time with monotonic component; not a NIC timestamp
	Number        int       // packet number in an offline capture; 0 for live capture
	CaptureLength int       // captured length from pcap/pcapng; 0 for live capture
	Length        int       // original packet length from pcap/pcapng; 0 for live capture
}

// Source is the input port for raw Ethernet frames.
// Frames returns received frames, while Errors returns non-blocking capture
// errors such as drops. Both channels are closed after Close.
type Source interface {
	Frames() <-chan Frame
	Errors() <-chan error
	Close() error
}
