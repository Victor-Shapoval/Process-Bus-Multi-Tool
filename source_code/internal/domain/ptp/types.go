// Package ptp implements the IEEE 1588-2008 (PTPv2) types and codec.
// It is used by the ptp_client and ptp_server modules.
package ptp

import (
	"fmt"
	"time"
)

// EtherType PTP (IEEE 1588, Annex F).
const EtherTypePTP uint16 = 0x88F7

// Timestamp is a PTP timestamp: 48-bit seconds plus 32-bit nanoseconds.
type Timestamp struct {
	Seconds     uint64 // only the lower 48 bits are used
	Nanoseconds uint32
}

// TimestampFromTime converts time.Time to a PTP Timestamp.
func TimestampFromTime(t time.Time) Timestamp {
	return Timestamp{
		Seconds:     uint64(t.Unix()),
		Nanoseconds: uint32(t.Nanosecond()),
	}
}

// TimestampFromTimeWithUTCOffset converts a UTC time.Time to a PTP timestamp
// on the PTP/TAI timescale, where Seconds = Unix seconds + currentUtcOffset.
func TimestampFromTimeWithUTCOffset(t time.Time, utcOffset int16) Timestamp {
	return Timestamp{
		Seconds:     uint64(t.Unix() + int64(utcOffset)),
		Nanoseconds: uint32(t.Nanosecond()),
	}
}

// ToTime converts a PTP Timestamp to time.Time.
func (ts Timestamp) ToTime() time.Time {
	return time.Unix(int64(ts.Seconds), int64(ts.Nanoseconds)).UTC()
}

// ToTimeWithUTCOffset converts a PTP/TAI timestamp to a UTC time.Time.
func (ts Timestamp) ToTimeWithUTCOffset(utcOffset int16) time.Time {
	return time.Unix(int64(ts.Seconds)-int64(utcOffset), int64(ts.Nanoseconds)).UTC()
}

// ClockIdentity is an 8-byte clock identifier (EUI-64).
type ClockIdentity [8]byte

// String returns the identifier in XX-XX-XX-XX-XX-XX-XX-XX notation.
func (ci ClockIdentity) String() string {
	return fmt.Sprintf("%02x-%02x-%02x-%02x-%02x-%02x-%02x-%02x",
		ci[0], ci[1], ci[2], ci[3], ci[4], ci[5], ci[6], ci[7])
}

// ClockIdentityFromMAC creates an EUI-64 from a 6-byte MAC address by inserting FF:FE.
func ClockIdentityFromMAC(mac [6]byte) ClockIdentity {
	return ClockIdentity{
		mac[0], mac[1], mac[2],
		0xFF, 0xFE,
		mac[3], mac[4], mac[5],
	}
}

// ClientClockIdentityFromMAC creates a separate identity for the PTP Client.
// Toggling the U/L bit ensures that a local Server and Client on the same NIC
// do not receive the same ClockIdentity.
func ClientClockIdentityFromMAC(mac [6]byte) ClockIdentity {
	identity := ClockIdentityFromMAC(mac)
	identity[0] ^= 0x02
	return identity
}

// PortIdentity is the unique identifier of a PTP port.
type PortIdentity struct {
	ClockIdentity ClockIdentity
	PortNumber    uint16
}

func (pi PortIdentity) String() string {
	return fmt.Sprintf("%s/%d", pi.ClockIdentity, pi.PortNumber)
}

// DelayMechanismName returns the short name of a delay mechanism.
func DelayMechanismName(mechanism uint8) string {
	switch mechanism {
	case DelayMechanismE2E:
		return "E2E"
	case DelayMechanismP2P:
		return "P2P"
	default:
		return fmt.Sprintf("Unknown (0x%02X)", mechanism)
	}
}

// ClockClassName returns a short description of the clockClass from Announce.
func ClockClassName(class uint8) string {
	switch class {
	case ClockClass6:
		return "Primary reference (synchronized)"
	case ClockClass7:
		return "Primary reference (holdover)"
	case ClockClass135:
		return "Traceable reference"
	case ClockClass248:
		return "Free-running oscillator"
	default:
		return "Profile-specific / reserved"
	}
}

// ClockAccuracyName returns a readable clockQuality accuracy value.
func ClockAccuracyName(accuracy uint8) string {
	switch accuracy {
	case ClockAccuracy25ns:
		return "25 ns"
	case ClockAccuracy100ns:
		return "100 ns"
	case ClockAccuracy250ns:
		return "250 ns"
	case ClockAccuracy1us:
		return "1 us"
	case ClockAccuracy2_5us:
		return "2.5 us"
	case ClockAccuracy10us:
		return "10 us"
	case ClockAccuracy25us:
		return "25 us"
	case ClockAccuracy100us:
		return "100 us"
	case ClockAccuracy250us:
		return "250 us"
	case ClockAccuracy1ms:
		return "1 ms"
	case ClockAccuracyUnknown:
		return "Unknown"
	default:
		return "Profile-specific / reserved"
	}
}

// ClockVarianceName returns a readable offsetScaledLogVariance value.
// Unlike clockAccuracy, this is a logarithmic metric rather than a duration.
func ClockVarianceName(variance uint16) string {
	if variance == ClockVarianceUnknown {
		return "Unknown"
	}
	return "Specified"
}

// TimeSourceName returns the name of the time source from Announce.
func TimeSourceName(source uint8) string {
	switch source {
	case TimeSourceAtomicClock:
		return "Atomic clock"
	case TimeSourceGPS:
		return "GPS"
	case TimeSourceTerrestrialRadio:
		return "Terrestrial radio"
	case TimeSourceSerialTimeCode:
		return "Serial time code"
	case TimeSourcePTP:
		return "PTP"
	case TimeSourceNTP:
		return "NTP"
	case TimeSourceHandSet:
		return "Hand set"
	case TimeSourceOther:
		return "Other"
	case TimeSourceInternalOscillator:
		return "Internal oscillator"
	default:
		return "Unknown / reserved"
	}
}

// ClockQuality describes clock quality (IEEE 1588-2008, Section 7.6.2).
type ClockQuality struct {
	ClockClass              uint8
	ClockAccuracy           uint8
	OffsetScaledLogVariance uint16
}

// Clock class constants (IEEE 1588-2008, Table 5).
const (
	ClockClass6   uint8 = 6   // synchronized primary reference
	ClockClass7   uint8 = 7   // primary reference in holdover
	ClockClass135 uint8 = 135 // traceable to a primary reference (NTP)
	ClockClass248 uint8 = 248 // free-running oscillator
)

// Clock accuracy constants (IEEE 1588-2008, Table 6).
const (
	ClockAccuracy25ns    uint8 = 0x20
	ClockAccuracy100ns   uint8 = 0x21
	ClockAccuracy250ns   uint8 = 0x22
	ClockAccuracy1us     uint8 = 0x23
	ClockAccuracy2_5us   uint8 = 0x24
	ClockAccuracy10us    uint8 = 0x25
	ClockAccuracy25us    uint8 = 0x26
	ClockAccuracy100us   uint8 = 0x27
	ClockAccuracy250us   uint8 = 0x28
	ClockAccuracy1ms     uint8 = 0x29
	ClockAccuracyUnknown uint8 = 0xFE
)

// offsetScaledLogVariance constants (IEEE 1588-2008, Section 7.6.3.3).
const (
	ClockVarianceGPS     uint16 = 0x4E5D // typical value for GPS-synchronized clocks
	ClockVarianceUnknown uint16 = 0xFFFF
)

// Time source constants (IEEE 1588-2008, Table 7).
const (
	TimeSourceAtomicClock        uint8 = 0x10
	TimeSourceGPS                uint8 = 0x20
	TimeSourceTerrestrialRadio   uint8 = 0x30
	TimeSourceSerialTimeCode     uint8 = 0x39
	TimeSourcePTP                uint8 = 0x40
	TimeSourceNTP                uint8 = 0x50
	TimeSourceHandSet            uint8 = 0x60
	TimeSourceOther              uint8 = 0x90
	TimeSourceInternalOscillator uint8 = 0xA0
)
