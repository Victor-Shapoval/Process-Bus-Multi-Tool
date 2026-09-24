package goose

import "net"

// These ranges are reserved for GOOSE by IEC 61850-8-1.

// AppIDMax is the upper bound of the GOOSE AppID range (IEC 61850-8-1, Annex C).
// Values 0x4000 and above are reserved for other protocols such as SV.
const AppIDMax uint16 = 0x3FFF

// IsValidMulticastMAC reports whether a MAC belongs to the GOOSE multicast range:
// the first four bytes are fixed at 01:0C:CD:01 and the lower 16 bits are in [0x0000..0x01FF].
func IsValidMulticastMAC(mac net.HardwareAddr) bool {
	if len(mac) != 6 {
		return false
	}
	if mac[0] != 0x01 || mac[1] != 0x0C || mac[2] != 0xCD || mac[3] != 0x01 {
		return false
	}
	low := uint16(mac[4])<<8 | uint16(mac[5])
	return low <= 0x01FF
}

// IsValidAppID reports whether an AppID is within the range permitted for GOOSE.
func IsValidAppID(id uint16) bool { return id <= AppIDMax }
