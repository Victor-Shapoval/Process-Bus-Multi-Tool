package ptp

import (
	"encoding/binary"
	"fmt"
)

const (
	timestampSize    = 10
	portIdentitySize = 10
	announceBodySize = 30
)

// EncodeHeader serializes the PTP header into dst (at least HeaderSize bytes).
func EncodeHeader(h Header, dst []byte) {
	dst[0] = (h.TransportSpecific << 4) | (h.MessageType & 0x0F)
	dst[1] = (h.MinorSdoId << 4) | (h.VersionPTP & 0x0F)
	binary.BigEndian.PutUint16(dst[2:4], h.MessageLength)
	dst[4] = h.DomainNumber
	dst[5] = 0
	binary.BigEndian.PutUint16(dst[6:8], h.FlagField)
	binary.BigEndian.PutUint64(dst[8:16], uint64(h.CorrectionField))
	binary.BigEndian.PutUint32(dst[16:20], h.Reserved)
	copy(dst[20:28], h.SourcePortIdentity.ClockIdentity[:])
	binary.BigEndian.PutUint16(dst[28:30], h.SourcePortIdentity.PortNumber)
	binary.BigEndian.PutUint16(dst[30:32], h.SequenceID)
	dst[32] = h.ControlField
	dst[33] = uint8(h.LogMessageInterval)
}

// DecodeHeader deserializes the PTP header from b.
func DecodeHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("ptp: buffer too short for header (%d < %d)", len(b), HeaderSize)
	}
	messageLength := binary.BigEndian.Uint16(b[2:4])
	if messageLength < HeaderSize {
		return Header{}, fmt.Errorf("ptp: invalid message length %d", messageLength)
	}
	if int(messageLength) > len(b) {
		return Header{}, fmt.Errorf("ptp: truncated message (%d < %d)", len(b), messageLength)
	}
	h := Header{
		TransportSpecific:  (b[0] >> 4) & 0x0F,
		MessageType:        b[0] & 0x0F,
		MinorSdoId:         (b[1] >> 4) & 0x0F,
		VersionPTP:         b[1] & 0x0F,
		MessageLength:      messageLength,
		DomainNumber:       b[4],
		FlagField:          binary.BigEndian.Uint16(b[6:8]),
		CorrectionField:    int64(binary.BigEndian.Uint64(b[8:16])),
		Reserved:           binary.BigEndian.Uint32(b[16:20]),
		SequenceID:         binary.BigEndian.Uint16(b[30:32]),
		ControlField:       b[32],
		LogMessageInterval: int8(b[33]),
	}
	copy(h.SourcePortIdentity.ClockIdentity[:], b[20:28])
	h.SourcePortIdentity.PortNumber = binary.BigEndian.Uint16(b[28:30])
	return h, nil
}

// encodeTimestamp serializes a PTP timestamp (10 bytes).
func encodeTimestamp(ts Timestamp, dst []byte) {
	binary.BigEndian.PutUint16(dst[0:2], uint16(ts.Seconds>>32))
	binary.BigEndian.PutUint32(dst[2:6], uint32(ts.Seconds))
	binary.BigEndian.PutUint32(dst[6:10], ts.Nanoseconds)
}

// DecodeTimestamp deserializes a PTP timestamp (10 bytes).
func DecodeTimestamp(b []byte) Timestamp {
	secHi := uint64(binary.BigEndian.Uint16(b[0:2]))
	secLo := uint64(binary.BigEndian.Uint32(b[2:6]))
	return Timestamp{
		Seconds:     (secHi << 32) | secLo,
		Nanoseconds: binary.BigEndian.Uint32(b[6:10]),
	}
}

func decodePortIdentity(b []byte) PortIdentity {
	var pi PortIdentity
	copy(pi.ClockIdentity[:], b[0:8])
	pi.PortNumber = binary.BigEndian.Uint16(b[8:10])
	return pi
}

// --- Encode functions (the client needs DelayReq / PDelayReq) ---

// EncodeDelayReq serializes DELAY_REQ.
func EncodeDelayReq(h Header, body DelayReqBody) []byte {
	total := HeaderSize + timestampSize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeTimestamp(body.OriginTimestamp, b[HeaderSize:])
	return b
}

// EncodePDelayReq serializes PDELAY_REQ (54 bytes).
func EncodePDelayReq(h Header, body PDelayReqBody) []byte {
	const reserved = 10
	total := HeaderSize + timestampSize + reserved
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeTimestamp(body.OriginTimestamp, b[HeaderSize:])
	return b
}

// --- Server encode functions: Announce, Sync, FollowUp, DelayResp, PDelayResp, PDelayRespFollowUp ---

// encodeAnnounceBody serializes the ANNOUNCE body (30 bytes) into dst.
func encodeAnnounceBody(dst []byte, body AnnounceBody) {
	off := 0
	encodeTimestamp(body.OriginTimestamp, dst[off:])
	off += timestampSize
	binary.BigEndian.PutUint16(dst[off:], uint16(body.CurrentUtcOffset))
	off += 2
	dst[off] = body.Reserved
	off++
	dst[off] = body.GrandmasterPriority1
	off++
	dst[off] = body.GrandmasterClockQuality.ClockClass
	off++
	dst[off] = body.GrandmasterClockQuality.ClockAccuracy
	off++
	binary.BigEndian.PutUint16(dst[off:], body.GrandmasterClockQuality.OffsetScaledLogVariance)
	off += 2
	dst[off] = body.GrandmasterPriority2
	off++
	copy(dst[off:], body.GrandmasterIdentity[:])
	off += 8
	binary.BigEndian.PutUint16(dst[off:], body.StepsRemoved)
	off += 2
	dst[off] = body.TimeSource
}

func encodePortIdentity(dst []byte, pi PortIdentity) {
	copy(dst[0:8], pi.ClockIdentity[:])
	binary.BigEndian.PutUint16(dst[8:10], pi.PortNumber)
}

// EncodeAnnounce serializes ANNOUNCE (header and body, without TLVs).
func EncodeAnnounce(h Header, body AnnounceBody) []byte {
	total := HeaderSize + announceBodySize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeAnnounceBody(b[HeaderSize:], body)
	return b
}

// EncodeAnnounceC372382011 serializes ANNOUNCE with a C37.238-2011 profile TLV
// followed by an ALTERNATE_TIME_OFFSET_INDICATOR TLV when requested.
func EncodeAnnounceC372382011(h Header, body AnnounceBody, tlv C37238TLV2011, alternate *AlternateTimeOffsetTLV) []byte {
	const c37TLVValueSize = 18
	const c37TLVSize = 4 + c37TLVValueSize
	alternateSize := 0
	if alternate != nil {
		alternateSize = 4 + alternateTimeOffsetValueSize(*alternate)
	}
	total := HeaderSize + announceBodySize + c37TLVSize + alternateSize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeAnnounceBody(b[HeaderSize:], body)

	off := HeaderSize + announceBodySize
	binary.BigEndian.PutUint16(b[off:], TLVOrganizationExtension)
	binary.BigEndian.PutUint16(b[off+2:], c37TLVValueSize)
	off += 4
	copy(b[off:], C37238OrgID[:])
	off += 3
	copy(b[off:], C37238OrgSubType2011[:])
	off += 3
	binary.BigEndian.PutUint16(b[off:], tlv.GrandmasterID)
	off += 2
	binary.BigEndian.PutUint32(b[off:], tlv.GrandmasterTimeInaccuracy)
	off += 4
	binary.BigEndian.PutUint32(b[off:], tlv.NetworkTimeInaccuracy)
	off += 4
	binary.BigEndian.PutUint16(b[off:], tlv.Reserved)
	off += 2

	if alternate != nil {
		encodeAlternateTimeOffset(b[off:], *alternate)
	}
	return b
}

// EncodeAnnounceC372382017 serializes ANNOUNCE with a C37.238-2017 profile TLV
// followed by an ALTERNATE_TIME_OFFSET_INDICATOR TLV when requested.
func EncodeAnnounceC372382017(h Header, body AnnounceBody, tlv C37238TLV2017, alternate *AlternateTimeOffsetTLV) []byte {
	const c37TLVValueSize = 18
	const c37TLVSize = 4 + c37TLVValueSize
	alternateSize := 0
	if alternate != nil {
		alternateSize = 4 + alternateTimeOffsetValueSize(*alternate)
	}
	total := HeaderSize + announceBodySize + c37TLVSize + alternateSize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeAnnounceBody(b[HeaderSize:], body)

	off := HeaderSize + announceBodySize
	binary.BigEndian.PutUint16(b[off:], TLVOrganizationExtension)
	binary.BigEndian.PutUint16(b[off+2:], c37TLVValueSize)
	off += 4
	copy(b[off:], C37238OrgID[:])
	off += 3
	copy(b[off:], C37238OrgSubType2017[:])
	off += 3
	binary.BigEndian.PutUint16(b[off:], tlv.GrandmasterID)
	off += 2
	binary.BigEndian.PutUint32(b[off:], tlv.Reserved)
	off += 4
	binary.BigEndian.PutUint32(b[off:], tlv.TotalTimeInaccuracy)
	off += 4
	binary.BigEndian.PutUint16(b[off:], tlv.Reserved2)
	off += 2

	if alternate != nil {
		encodeAlternateTimeOffset(b[off:], *alternate)
	}
	return b
}

func alternateTimeOffsetValueSize(tlv AlternateTimeOffsetTLV) int {
	// keyField + currentOffset + jumpSeconds + 48-bit time + PTPText length.
	size := 1 + 4 + 4 + 6 + 1 + len([]byte(tlv.DisplayName))
	if size%2 != 0 {
		size++
	}
	return size
}

func encodeAlternateTimeOffset(dst []byte, tlv AlternateTimeOffsetTLV) {
	valueSize := alternateTimeOffsetValueSize(tlv)
	binary.BigEndian.PutUint16(dst[0:2], TLVAlternateTimeOffset)
	binary.BigEndian.PutUint16(dst[2:4], uint16(valueSize))
	off := 4
	dst[off] = tlv.KeyField
	off++
	binary.BigEndian.PutUint32(dst[off:], uint32(tlv.CurrentOffset))
	off += 4
	binary.BigEndian.PutUint32(dst[off:], uint32(tlv.JumpSeconds))
	off += 4
	dst[off] = byte(tlv.TimeOfNextJump >> 40)
	dst[off+1] = byte(tlv.TimeOfNextJump >> 32)
	dst[off+2] = byte(tlv.TimeOfNextJump >> 24)
	dst[off+3] = byte(tlv.TimeOfNextJump >> 16)
	dst[off+4] = byte(tlv.TimeOfNextJump >> 8)
	dst[off+5] = byte(tlv.TimeOfNextJump)
	off += 6
	name := []byte(tlv.DisplayName)
	dst[off] = byte(len(name))
	off++
	copy(dst[off:], name)
}

// EncodeSync serializes SYNC.
func EncodeSync(h Header, body SyncBody) []byte {
	total := HeaderSize + timestampSize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeTimestamp(body.OriginTimestamp, b[HeaderSize:])
	return b
}

// EncodeFollowUp serializes FOLLOW_UP.
func EncodeFollowUp(h Header, body FollowUpBody) []byte {
	total := HeaderSize + timestampSize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	encodeTimestamp(body.PreciseOriginTimestamp, b[HeaderSize:])
	return b
}

// EncodeDelayResp serializes DELAY_RESP.
func EncodeDelayResp(h Header, body DelayRespBody) []byte {
	total := HeaderSize + timestampSize + portIdentitySize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	off := HeaderSize
	encodeTimestamp(body.ReceiveTimestamp, b[off:])
	off += timestampSize
	encodePortIdentity(b[off:], body.RequestingPortIdentity)
	return b
}

// EncodePDelayResp serializes PDELAY_RESP.
func EncodePDelayResp(h Header, body PDelayRespBody) []byte {
	total := HeaderSize + timestampSize + portIdentitySize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	off := HeaderSize
	encodeTimestamp(body.RequestReceiptTimestamp, b[off:])
	off += timestampSize
	encodePortIdentity(b[off:], body.RequestingPortIdentity)
	return b
}

// EncodePDelayRespFollowUp serializes PDELAY_RESP_FOLLOW_UP.
func EncodePDelayRespFollowUp(h Header, body PDelayRespFollowUpBody) []byte {
	total := HeaderSize + timestampSize + portIdentitySize
	h.MessageLength = uint16(total)
	b := make([]byte, total)
	EncodeHeader(h, b)
	off := HeaderSize
	encodeTimestamp(body.ResponseOriginTimestamp, b[off:])
	off += timestampSize
	encodePortIdentity(b[off:], body.RequestingPortIdentity)
	return b
}

// DecodeDelayReqBody decodes the DELAY_REQ body.
func DecodeDelayReqBody(b []byte) (DelayReqBody, error) {
	if len(b) < timestampSize {
		return DelayReqBody{}, fmt.Errorf("ptp: buffer too short for DELAY_REQ body (%d < %d)", len(b), timestampSize)
	}
	return DelayReqBody{OriginTimestamp: DecodeTimestamp(b)}, nil
}

// DecodePDelayReqBody decodes the PDELAY_REQ body.
func DecodePDelayReqBody(b []byte) (PDelayReqBody, error) {
	if len(b) < timestampSize {
		return PDelayReqBody{}, fmt.Errorf("ptp: buffer too short for PDELAY_REQ body (%d < %d)", len(b), timestampSize)
	}
	return PDelayReqBody{OriginTimestamp: DecodeTimestamp(b)}, nil
}

// --- Client decode functions: Sync, FollowUp, Announce, DelayResp, PDelayResp, PDelayRespFollowUp ---

// DecodeSyncBody decodes the SYNC body.
func DecodeSyncBody(b []byte) (SyncBody, error) {
	if len(b) < timestampSize {
		return SyncBody{}, fmt.Errorf("ptp: buffer too short for SYNC body (%d < %d)", len(b), timestampSize)
	}
	return SyncBody{OriginTimestamp: DecodeTimestamp(b)}, nil
}

// DecodeFollowUpBody decodes the FOLLOW_UP body.
func DecodeFollowUpBody(b []byte) (FollowUpBody, error) {
	if len(b) < timestampSize {
		return FollowUpBody{}, fmt.Errorf("ptp: buffer too short for FOLLOW_UP body (%d < %d)", len(b), timestampSize)
	}
	return FollowUpBody{PreciseOriginTimestamp: DecodeTimestamp(b)}, nil
}

// DecodeDelayRespBody decodes the DELAY_RESP body.
func DecodeDelayRespBody(b []byte) (DelayRespBody, error) {
	need := timestampSize + portIdentitySize
	if len(b) < need {
		return DelayRespBody{}, fmt.Errorf("ptp: buffer too short for DELAY_RESP body (%d < %d)", len(b), need)
	}
	return DelayRespBody{
		ReceiveTimestamp:       DecodeTimestamp(b),
		RequestingPortIdentity: decodePortIdentity(b[timestampSize:]),
	}, nil
}

// DecodeAnnounceBody decodes the ANNOUNCE body (30 bytes).
func DecodeAnnounceBody(b []byte) (AnnounceBody, error) {
	if len(b) < announceBodySize {
		return AnnounceBody{}, fmt.Errorf("ptp: buffer too short for ANNOUNCE body (%d < %d)", len(b), announceBodySize)
	}
	a := AnnounceBody{
		OriginTimestamp:      DecodeTimestamp(b[0:]),
		CurrentUtcOffset:     int16(binary.BigEndian.Uint16(b[10:12])),
		Reserved:             b[12],
		GrandmasterPriority1: b[13],
		GrandmasterClockQuality: ClockQuality{
			ClockClass:              b[14],
			ClockAccuracy:           b[15],
			OffsetScaledLogVariance: binary.BigEndian.Uint16(b[16:18]),
		},
		GrandmasterPriority2: b[18],
		StepsRemoved:         binary.BigEndian.Uint16(b[27:29]),
		TimeSource:           b[29],
	}
	copy(a.GrandmasterIdentity[:], b[19:27])
	return a, nil
}

// DecodeAnnounceProfileTLVs decodes the TLVs following the fixed 30-byte
// Announce body. Unknown TLV types and organization identifiers are ignored.
func DecodeAnnounceProfileTLVs(b []byte) (AnnounceProfileTLVs, error) {
	if len(b) < announceBodySize {
		return AnnounceProfileTLVs{}, fmt.Errorf("ptp: buffer too short for ANNOUNCE body (%d < %d)", len(b), announceBodySize)
	}

	var result AnnounceProfileTLVs
	for remaining := b[announceBodySize:]; len(remaining) != 0; {
		if len(remaining) < 4 {
			return AnnounceProfileTLVs{}, fmt.Errorf("ptp: truncated TLV header (%d < 4)", len(remaining))
		}
		tlvType := binary.BigEndian.Uint16(remaining[0:2])
		valueLength := int(binary.BigEndian.Uint16(remaining[2:4]))
		if valueLength > len(remaining)-4 {
			return AnnounceProfileTLVs{}, fmt.Errorf("ptp: truncated TLV value (%d < %d)", len(remaining)-4, valueLength)
		}
		value := remaining[4 : 4+valueLength]

		switch tlvType {
		case TLVOrganizationExtension:
			if err := decodeC37238OrganizationTLV(value, &result); err != nil {
				return AnnounceProfileTLVs{}, err
			}
		case TLVAlternateTimeOffset:
			if result.AlternateTimeOffset != nil {
				return AnnounceProfileTLVs{}, fmt.Errorf("ptp: duplicate ALTERNATE_TIME_OFFSET_INDICATOR TLV")
			}
			alternate, err := decodeAlternateTimeOffset(value)
			if err != nil {
				return AnnounceProfileTLVs{}, err
			}
			result.AlternateTimeOffset = &alternate
		}

		remaining = remaining[4+valueLength:]
	}
	return result, nil
}

func decodeC37238OrganizationTLV(value []byte, result *AnnounceProfileTLVs) error {
	if len(value) < 3 || [3]byte(value[0:3]) != C37238OrgID {
		return nil
	}
	if len(value) != 18 {
		return fmt.Errorf("ptp: invalid C37.238 organization TLV length %d", len(value))
	}
	if result.C37238Version != C37238Disabled {
		return fmt.Errorf("ptp: duplicate C37.238 organization TLV")
	}

	subtype := [3]byte(value[3:6])
	switch subtype {
	case C37238OrgSubType2011:
		tlv := C37238TLV2011{
			GrandmasterID:             binary.BigEndian.Uint16(value[6:8]),
			GrandmasterTimeInaccuracy: binary.BigEndian.Uint32(value[8:12]),
			NetworkTimeInaccuracy:     binary.BigEndian.Uint32(value[12:16]),
			Reserved:                  binary.BigEndian.Uint16(value[16:18]),
		}
		result.C37238Version = C37238Version2011
		result.C372382011 = &tlv
	case C37238OrgSubType2017:
		tlv := C37238TLV2017{
			GrandmasterID:       binary.BigEndian.Uint16(value[6:8]),
			Reserved:            binary.BigEndian.Uint32(value[8:12]),
			TotalTimeInaccuracy: binary.BigEndian.Uint32(value[12:16]),
			Reserved2:           binary.BigEndian.Uint16(value[16:18]),
		}
		result.C37238Version = C37238Version2017
		result.C372382017 = &tlv
	}
	return nil
}

func decodeAlternateTimeOffset(value []byte) (AlternateTimeOffsetTLV, error) {
	const fixedValueSize = 16
	if len(value) < fixedValueSize {
		return AlternateTimeOffsetTLV{}, fmt.Errorf("ptp: invalid ALTERNATE_TIME_OFFSET_INDICATOR TLV length %d", len(value))
	}
	nameLength := int(value[15])
	expectedLength := fixedValueSize + nameLength
	if expectedLength%2 != 0 {
		expectedLength++
	}
	if len(value) != expectedLength {
		return AlternateTimeOffsetTLV{}, fmt.Errorf("ptp: invalid ALTERNATE_TIME_OFFSET_INDICATOR display name length %d for TLV length %d", nameLength, len(value))
	}
	timeOfNextJump := uint64(value[9])<<40 |
		uint64(value[10])<<32 |
		uint64(value[11])<<24 |
		uint64(value[12])<<16 |
		uint64(value[13])<<8 |
		uint64(value[14])
	return AlternateTimeOffsetTLV{
		KeyField:       value[0],
		CurrentOffset:  int32(binary.BigEndian.Uint32(value[1:5])),
		JumpSeconds:    int32(binary.BigEndian.Uint32(value[5:9])),
		TimeOfNextJump: timeOfNextJump,
		DisplayName:    string(value[16 : 16+nameLength]),
	}, nil
}

// DecodePDelayRespBody decodes the PDELAY_RESP body.
func DecodePDelayRespBody(b []byte) (PDelayRespBody, error) {
	need := timestampSize + portIdentitySize
	if len(b) < need {
		return PDelayRespBody{}, fmt.Errorf("ptp: buffer too short for PDELAY_RESP body (%d < %d)", len(b), need)
	}
	return PDelayRespBody{
		RequestReceiptTimestamp: DecodeTimestamp(b),
		RequestingPortIdentity:  decodePortIdentity(b[timestampSize:]),
	}, nil
}

// DecodePDelayRespFollowUpBody decodes the PDELAY_RESP_FOLLOW_UP body.
func DecodePDelayRespFollowUpBody(b []byte) (PDelayRespFollowUpBody, error) {
	need := timestampSize + portIdentitySize
	if len(b) < need {
		return PDelayRespFollowUpBody{}, fmt.Errorf("ptp: buffer too short for PDELAY_RESP_FOLLOW_UP body (%d < %d)", len(b), need)
	}
	return PDelayRespFollowUpBody{
		ResponseOriginTimestamp: DecodeTimestamp(b),
		RequestingPortIdentity:  decodePortIdentity(b[timestampSize:]),
	}, nil
}
