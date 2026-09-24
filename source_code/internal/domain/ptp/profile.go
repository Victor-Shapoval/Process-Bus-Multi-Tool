package ptp

import "math"

// C37238Version specifies the wire version of the IEEE C37.238 Organization Extension TLV.
type C37238Version uint8

const (
	C37238Disabled C37238Version = iota
	C37238Version2011
	C37238Version2017
)

func (v C37238Version) String() string {
	switch v {
	case C37238Version2011:
		return "2011"
	case C37238Version2017:
		return "2017"
	default:
		return "disabled"
	}
}

// ParseC37238Version converts a configuration value to the profile wire version.
func ParseC37238Version(value string) (C37238Version, bool) {
	switch value {
	case "2011":
		return C37238Version2011, true
	case "2017":
		return C37238Version2017, true
	default:
		return C37238Disabled, false
	}
}

// Profile describes the PTP profile parameters.
type Profile struct {
	Name                   string
	DomainNumber           uint8
	DelayMechanism         uint8
	LogSyncInterval        int8  // log2(seconds): 0 = 1 s, -4 = 62.5 ms
	LogAnnounceInterval    int8  // log2(seconds)
	AnnounceReceiptTimeout uint8 // multiple of the Announce interval
	LogDelayReqInterval    int8  // log2(seconds) for E2E delay_req
	LogPDelayReqInterval   int8  // log2(seconds) for P2P pdelay_req
	TransportSpecific      uint8 // majorSdoId; 0 for IEEE 1588/C37.238, 1 for 802.1AS
	Priority1              uint8
	Priority2              uint8
	// Server (grandmaster) fields:
	ClockClass    uint8         // clockClass (IEEE 1588-2008, Table 5)
	ClockAccuracy uint8         // clockAccuracy (IEEE 1588-2008, Table 6)
	ClockVariance uint16        // offsetScaledLogVariance
	TimeSource    uint8         // timeSource (IEEE 1588-2008, Table 7)
	FlagField     uint16        // flagField for ANNOUNCE (PTP_TIMESCALE | UTC_VALID | ...)
	C37238Version C37238Version // profile TLV version; disabled for the Default Profile
}

// SyncInterval returns the Sync interval in seconds.
func (p *Profile) SyncInterval() float64 {
	return math.Pow(2, float64(p.LogSyncInterval))
}

// AnnounceInterval returns the Announce interval in seconds.
func (p *Profile) AnnounceInterval() float64 {
	return math.Pow(2, float64(p.LogAnnounceInterval))
}

// AnnounceTimeoutDuration returns the Announce timeout in seconds.
func (p *Profile) AnnounceTimeoutDuration() float64 {
	return float64(p.AnnounceReceiptTimeout) * p.AnnounceInterval()
}

// DelayReqInterval returns the delay request interval in seconds.
func (p *Profile) DelayReqInterval() float64 {
	if p.DelayMechanism == DelayMechanismP2P {
		return math.Pow(2, float64(p.LogPDelayReqInterval))
	}
	return math.Pow(2, float64(p.LogDelayReqInterval))
}

// DefaultProfile — IEEE 1588-2008 default profile (domain 0, E2E, 1s sync).
var DefaultProfile = Profile{
	Name:                   "default",
	DomainNumber:           0,
	DelayMechanism:         DelayMechanismE2E,
	LogSyncInterval:        0, // 1 s
	LogAnnounceInterval:    1, // 2 s
	AnnounceReceiptTimeout: 3, // 3 × 2 s = 6 s
	LogDelayReqInterval:    0, // 1 s
	LogPDelayReqInterval:   0, // 1 s (unused in E2E)
	TransportSpecific:      0,
	Priority1:              128,
	Priority2:              128,
	ClockClass:             ClockClass6,
	ClockAccuracy:          ClockAccuracy100ns,
	ClockVariance:          ClockVarianceGPS,
	TimeSource:             TimeSourceInternalOscillator,
	FlagField:              FlagPTPTimescale | FlagCurrentUtcOffsetValid,
	C37238Version:          C37238Disabled,
}

// PowerProfile defines the base IEEE C37.238 Power Profile parameters (L2, P2P).
// It defaults to the C37.238-2011 variant compatible with early protection relays.
var PowerProfile = Profile{
	Name:                   "power",
	DomainNumber:           0,
	DelayMechanism:         DelayMechanismP2P,
	LogSyncInterval:        0, // 1 s
	LogAnnounceInterval:    1, // 2 s
	AnnounceReceiptTimeout: 3, // 3 × 2 s = 6 s
	LogDelayReqInterval:    0, // unused in P2P
	LogPDelayReqInterval:   0, // 1 s
	TransportSpecific:      0, // C37.238 uses the default PTP SDO; 1 is reserved for 802.1AS/gPTP
	Priority1:              128,
	Priority2:              128,
	ClockClass:             ClockClass6,
	ClockAccuracy:          ClockAccuracy100ns,
	ClockVariance:          ClockVarianceGPS,
	TimeSource:             TimeSourceGPS,
	FlagField:              FlagPTPTimescale | FlagCurrentUtcOffsetValid | FlagTimeTraceable | FlagFrequencyTraceable,
	C37238Version:          C37238Version2011,
}
