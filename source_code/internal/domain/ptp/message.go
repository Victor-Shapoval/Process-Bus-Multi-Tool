package ptp

// PTP message types (IEEE 1588-2008, Table 19).
const (
	MsgSync               uint8 = 0x0
	MsgDelayReq           uint8 = 0x1
	MsgPDelayReq          uint8 = 0x2
	MsgPDelayResp         uint8 = 0x3
	MsgFollowUp           uint8 = 0x8
	MsgDelayResp          uint8 = 0x9
	MsgPDelayRespFollowUp uint8 = 0xA
	MsgAnnounce           uint8 = 0xB
	MsgSignaling          uint8 = 0xC
	MsgManagement         uint8 = 0xD
)

// Flag field bits (IEEE 1588-2008, Table 20).
const (
	FlagAlternateMaster       uint16 = 0x0100
	FlagTwoStep               uint16 = 0x0200
	FlagUnicast               uint16 = 0x0400
	FlagProfileSpecific1      uint16 = 0x2000
	FlagProfileSpecific2      uint16 = 0x4000
	FlagLeap61                uint16 = 0x0001
	FlagLeap59                uint16 = 0x0002
	FlagCurrentUtcOffsetValid uint16 = 0x0004
	FlagPTPTimescale          uint16 = 0x0008
	FlagTimeTraceable         uint16 = 0x0010
	FlagFrequencyTraceable    uint16 = 0x0020
)

// Control field values (IEEE 1588-2008, Table 23).
const (
	ControlSync       uint8 = 0x00
	ControlDelayReq   uint8 = 0x01
	ControlFollowUp   uint8 = 0x02
	ControlDelayResp  uint8 = 0x03
	ControlManagement uint8 = 0x04
	ControlOther      uint8 = 0x05
)

const PTPVersion2 uint8 = 0x02

// HeaderSize is the fixed size of a PTP message header.
const HeaderSize = 34

// Header is the common PTP message header (34 bytes).
type Header struct {
	MessageType        uint8
	TransportSpecific  uint8
	VersionPTP         uint8
	MessageLength      uint16
	DomainNumber       uint8
	MinorSdoId         uint8
	FlagField          uint16
	CorrectionField    int64 // nanoseconds × 2^16
	Reserved           uint32
	SourcePortIdentity PortIdentity
	SequenceID         uint16
	ControlField       uint8
	LogMessageInterval int8
}

// AnnounceBody is the ANNOUNCE message body (30 bytes).
type AnnounceBody struct {
	OriginTimestamp         Timestamp
	CurrentUtcOffset        int16
	Reserved                uint8
	GrandmasterPriority1    uint8
	GrandmasterClockQuality ClockQuality
	GrandmasterPriority2    uint8
	GrandmasterIdentity     ClockIdentity
	StepsRemoved            uint16
	TimeSource              uint8
}

// SyncBody is the SYNC body (10 bytes).
type SyncBody struct {
	OriginTimestamp Timestamp
}

// FollowUpBody is the FOLLOW_UP body (10 bytes).
type FollowUpBody struct {
	PreciseOriginTimestamp Timestamp
}

// DelayReqBody is the DELAY_REQ body (10 bytes).
type DelayReqBody struct {
	OriginTimestamp Timestamp
}

// DelayRespBody is the DELAY_RESP body (20 bytes).
type DelayRespBody struct {
	ReceiveTimestamp       Timestamp
	RequestingPortIdentity PortIdentity
}

// PDelayReqBody is the PDELAY_REQ body (20 bytes).
type PDelayReqBody struct {
	OriginTimestamp Timestamp
}

// PDelayRespBody is the PDELAY_RESP body (20 bytes).
type PDelayRespBody struct {
	RequestReceiptTimestamp Timestamp
	RequestingPortIdentity  PortIdentity
}

// PDelayRespFollowUpBody is the PDELAY_RESP_FOLLOW_UP body (20 bytes).
type PDelayRespFollowUpBody struct {
	ResponseOriginTimestamp Timestamp
	RequestingPortIdentity  PortIdentity
}

// Delay mechanisms (IEEE 1588-2008, Table 9).
const (
	DelayMechanismE2E uint8 = 0x01
	DelayMechanismP2P uint8 = 0x02
)

// TLV types.
const (
	TLVPathTrace             uint16 = 0x0008
	TLVOrganizationExtension uint16 = 0x0003
	TLVAlternateTimeOffset   uint16 = 0x0009
)

// PTP multicast addresses (IEEE 1588-2008, Annex F).
var (
	PrimaryMulticastMAC = [6]byte{0x01, 0x1B, 0x19, 0x00, 0x00, 0x00}
	PDelayMulticastMAC  = [6]byte{0x01, 0x80, 0xC2, 0x00, 0x00, 0x0E}
)

// PTP UDP ports.
const (
	UDPEventPort   = 319
	UDPGeneralPort = 320
)

// PTP UDP multicast groups.
const (
	PrimaryMulticastIP = "224.0.1.129"
	PDelayMulticastIP  = "224.0.0.107"
)

// C37238OrgID — OUI IEEE C37.238 (1C:12:9D).
var C37238OrgID = [3]byte{0x1C, 0x12, 0x9D}

var (
	// C37238OrgSubType2011 is the IEEE C37.238-2011 organizationSubType.
	C37238OrgSubType2011 = [3]byte{0x00, 0x00, 0x01}
	// C37238OrgSubType2017 is the IEEE C37.238-2017 organizationSubType.
	C37238OrgSubType2017 = [3]byte{0x00, 0x00, 0x02}
)

// C37238TLV2011 contains the IEEE C37.238-2011 Organization Extension TLV fields.
type C37238TLV2011 struct {
	GrandmasterID             uint16
	GrandmasterTimeInaccuracy uint32
	NetworkTimeInaccuracy     uint32
	Reserved                  uint16
}

// C37238TLV2017 contains the IEEE C37.238-2017 Organization Extension TLV fields.
type C37238TLV2017 struct {
	GrandmasterID       uint16
	Reserved            uint32
	TotalTimeInaccuracy uint32
	Reserved2           uint16
}

// AlternateTimeOffsetTLV describes an ALTERNATE_TIME_OFFSET_INDICATOR TLV.
// TimeOfNextJump occupies 48 bits on the wire.
type AlternateTimeOffsetTLV struct {
	KeyField       uint8
	CurrentOffset  int32
	JumpSeconds    int32
	TimeOfNextJump uint64
	DisplayName    string
}

// AnnounceProfileTLVs contains the recognized profile-specific TLVs appended
// to an Announce message. Exactly one C37.238 organization TLV is allowed.
type AnnounceProfileTLVs struct {
	C37238Version       C37238Version
	C372382011          *C37238TLV2011
	C372382017          *C37238TLV2017
	AlternateTimeOffset *AlternateTimeOffsetTLV
}
