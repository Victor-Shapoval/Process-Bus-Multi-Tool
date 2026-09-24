// Package sv contains domain types and ports for IEC 61850-9-2LE Sampled Values.
package sv

import (
	"net"
	"time"

	"pbmt/internal/domain/ethernet"
)

// EtherTypeSV is the SV EtherType specified by IEC 61850-9-2.
const EtherType = 0x88BA

const SeqDataLen = NumChannels * 8

const seqDataLen = SeqDataLen

// Channel indexes in the Channels and Quality arrays (IEC 61850-9-2LE, 8 channels).
const (
	ChIa = 0 // phase A current
	ChIb = 1 // phase B current
	ChIc = 2 // phase C current
	ChIn = 3 // neutral current
	ChUa = 4 // phase A voltage
	ChUb = 5 // phase B voltage
	ChUc = 6 // phase C voltage
	ChUn = 7 // neutral voltage
)

var ChannelNames = [NumChannels]string{"Ia", "Ib", "Ic", "In", "Ua", "Ub", "Uc", "Un"}

// NumChannels is the number of channels in an IEC 61850-9-2LE ASDU.
const NumChannels = 8

// IEC 61850-9-2LE scale factors:
//   - current: 1 mA/bit, divide by 1000 to obtain amperes
//   - voltage: 10 mV/bit, divide by 100 to obtain volts
const (
	CurrentScale = 1000.0 // raw → A
	VoltageScale = 100.0  // raw → V
)

// SmpMod is the sampling mode (IEC 61850-9-2 Ed2).
type SmpMod uint8

const (
	SmpModSamplesPerPeriod SmpMod = 0 // SPC: samples per cycle (default 9-2LE)
	SmpModSamplesPerSecond SmpMod = 1 // FPC: fixed samples per second
	SmpModSecondsPerSample SmpMod = 2 // APC: one sample per N seconds
)

func (m SmpMod) String() string {
	switch m {
	case SmpModSamplesPerPeriod:
		return "spc"
	case SmpModSamplesPerSecond:
		return "fps"
	case SmpModSecondsPerSample:
		return "apc"
	default:
		return "unknown"
	}
}

// SmpSynch is the sample synchronization flag (IEC 61850-9-2).
type SmpSynch uint8

const (
	SmpSynchNone   SmpSynch = 0 // unsynchronized
	SmpSynchLocal  SmpSynch = 1 // local source (GPS without PTP)
	SmpSynchGlobal SmpSynch = 2 // global source (PTP / IEEE 1588)
)

func (s SmpSynch) String() string {
	switch s {
	case SmpSynchNone:
		return "none"
	case SmpSynchLocal:
		return "local"
	case SmpSynchGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Quality is the sample quality (IEC 61850-7-3, 32 bits).
type Quality uint32

// IEC 61850-7-3 quality bits.
const (
	QualityValid           Quality = 0 // validity = 0 → good
	QualityReserved        Quality = 1 // validity = 1 → reserved
	QualityInvalid         Quality = 2 // validity = 2 → invalid
	QualityQuestionable    Quality = 3 // validity = 3 → questionable
	QualityOverflow        Quality = 1 << 2
	QualityOutOfRange      Quality = 1 << 3
	QualityBadReference    Quality = 1 << 4
	QualityOscillatory     Quality = 1 << 5
	QualityFailure         Quality = 1 << 6
	QualityOldData         Quality = 1 << 7
	QualityInconsistent    Quality = 1 << 8
	QualityInaccurate      Quality = 1 << 9
	QualitySubstituted     Quality = 1 << 10 // source: 0 = process, 1 = substituted
	QualityTest            Quality = 1 << 11
	QualityOperatorBlocked Quality = 1 << 12
	QualityDerived         Quality = 1 << 13
	QualityMask            Quality = (1 << 14) - 1
)

func (q Quality) Flags() []string {
	var out []string
	switch uint32(q) & 0x3 {
	case 1:
		out = append(out, "ReservedValidity")
	case 2:
		out = append(out, "Invalid")
	case 3:
		out = append(out, "Questionable")
	}
	bits := []struct {
		b Quality
		n string
	}{
		{QualityOverflow, "Overflow"},
		{QualityOutOfRange, "OutOfRange"},
		{QualityBadReference, "BadReference"},
		{QualityOscillatory, "Oscillatory"},
		{QualityFailure, "Failure"},
		{QualityOldData, "OldData"},
		{QualityInconsistent, "Inconsistent"},
		{QualityInaccurate, "Inaccurate"},
		{QualitySubstituted, "Substituted"},
		{QualityTest, "Test"},
		{QualityOperatorBlocked, "OperatorBlocked"},
		{QualityDerived, "Derived"},
	}
	for _, bit := range bits {
		if q&bit.b != 0 {
			out = append(out, bit.n)
		}
	}
	return out
}

// ASDU is one Application Service Data Unit from an SV stream (IEC 61850-9-2LE).
type ASDU struct {
	// Ethernet header
	DstMAC net.HardwareAddr
	SrcMAC net.HardwareAddr
	VLAN   *ethernet.VLANTag // nil = untagged

	// APDU header
	AppID      uint16
	APDULength int
	Reserved1  uint16
	Reserved2  uint16
	Simulation bool // S bit in Reserved1 (Ed2.1)

	// ASDU fields
	SvID     string      // [0x80] SV stream identifier
	DatSet   string      // [0x81] data set reference (optional)
	SmpCnt   uint16      // [0x82] sample counter (0..65535, wraps)
	ConfRev  uint32      // [0x83] configuration revision
	RefrTm   time.Time   // [0x84] sample UtcTime (optional)
	RefrTmQ  TimeQuality // refrTm timestamp quality
	SmpSynch SmpSynch    // [0x85] synchronization
	SmpRate  uint16      // [0x86] samples per period (80/256; 0 = not transmitted)
	SmpMod   SmpMod      // [0x88] sampling mode (Ed2; defaults to SPC when 0)

	// 8 IEC 61850-9-2LE channels (4 currents and 4 voltages)
	Channels [NumChannels]int32
	Quality  [NumChannels]Quality
	SeqData  []byte

	HasSvID     bool
	HasSmpCnt   bool
	HasConfRev  bool
	HasSmpSynch bool
	HasSeqData  bool
	HasRefrTm   bool
	HasSmpRate  bool
	HasSmpMod   bool
}

// TimeQuality is the quality of a UtcTime timestamp (IEC 61850-7-2).
// It duplicates goose.TimeQuality so that domain packages remain independent.
type TimeQuality struct {
	LeapSecondsKnown     bool
	ClockFailure         bool
	ClockNotSynchronized bool
	Accuracy             uint8 // 0..24 = 2^-n s; 31 = unspecified
}

const AccuracyUnspecified uint8 = 31

func (q TimeQuality) Valid() bool {
	return !q.ClockFailure && !q.ClockNotSynchronized
}

func (q TimeQuality) Flags() []string {
	var out []string
	if q.LeapSecondsKnown {
		out = append(out, "LeapSecondsKnown")
	}
	if q.ClockFailure {
		out = append(out, "ClockFailure")
	}
	if q.ClockNotSynchronized {
		out = append(out, "ClockNotSynchronized")
	}
	return out
}

func (a ASDU) PhysicalValue(ch int) float64 {
	if ch >= ChUa {
		return float64(a.Channels[ch]) / VoltageScale
	}
	return float64(a.Channels[ch]) / CurrentScale
}

func (a ASDU) PhysicalUnit(ch int) string {
	if ch >= ChUa {
		return "V"
	}
	return "A"
}

type DecodeWarning struct {
	Message string

	// ASDUScoped distinguishes a warning about one ASDU from a warning about
	// the enclosing Ethernet/APDU frame. ASDUIndex is valid only when this flag
	// is set.
	ASDUScoped bool
	ASDUIndex  int

	// RejectLive marks damage which is useful to report during offline analysis
	// but makes the sample unsafe for live measurements and watchdog updates.
	RejectLive bool
}

type DecodedFrame struct {
	ASDUs        []ASDU
	Warnings     []DecodeWarning
	NonSV        bool
	EtherType    uint16
	ActualASDUs  int
	DeclaredASDU int
}
