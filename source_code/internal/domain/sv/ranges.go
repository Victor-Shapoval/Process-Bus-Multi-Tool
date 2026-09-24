package sv

import "net"

// The ranges are reserved by IEC 61850-8-1 for SV (Annex C).

// SV AppID range: 0x4000..0x7FFF (IEC 61850-8-1, Annex C).
const (
	AppIDMin uint16 = 0x4000
	AppIDMax uint16 = 0x7FFF
)

// IsValidMulticastMAC reports whether a MAC belongs to the SV multicast range:
// the first 4 bytes are fixed (01:0C:CD:04), and the lower 16 bits are in [0x0000..0x01FF].
func IsValidMulticastMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 {
		return false
	}
	if mac[0] != 0x01 || mac[1] != 0x0C || mac[2] != 0xCD || mac[3] != 0x04 {
		return false
	}
	low := uint16(mac[4])<<8 | uint16(mac[5])
	return low <= 0x01FF
}

// IsValidAppID reports whether an AppID is in the SV range (0x4000..0x7FFF).
func IsValidAppID(id uint16) bool { return id >= AppIDMin && id <= AppIDMax }
