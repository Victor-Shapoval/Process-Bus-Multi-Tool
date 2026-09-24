package goose

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	"pbmt/internal/domain/ethernet"
)

// Decoding errors.
var (
	ErrFrameTooShort = errors.New("goose: frame too short")
	ErrNotGOOSE      = errors.New("goose: not a GOOSE frame")
	ErrBadBER        = errors.New("goose: invalid BER encoding")
	ErrBadPDU        = errors.New("goose: malformed goosePDU")
)

const (
	fieldGocbRef uint16 = 1 << iota
	fieldTimeAllowedToLive
	fieldDatSet
	fieldGoID
	fieldTimestamp
	fieldStNum
	fieldSqNum
	fieldTest
	fieldConfRev
	fieldNdsCom
	fieldNumDatSetEntries
	fieldAllData
)

const requiredGOOSEFields = fieldGocbRef |
	fieldTimeAllowedToLive |
	fieldDatSet |
	fieldTimestamp |
	fieldStNum |
	fieldSqNum |
	fieldConfRev |
	fieldNumDatSetEntries |
	fieldAllData

var requiredGOOSEFieldTags = [...]byte{0x80, 0x81, 0x82, 0x84, 0x85, 0x86, 0x88, 0x8A, 0xAB}

// Decode parses a raw Ethernet frame and extracts a GOOSE PDU.
// It is a pure function with no I/O or global-state allocation.
// The returned PDU owns copies of the bytes backing every string and []byte field.
func Decode(frame []byte) (*PDU, error) {
	if len(frame) < 14 {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooShort, len(frame))
	}

	pdu := &PDU{
		DstMAC: net.HardwareAddr(append([]byte(nil), frame[0:6]...)),
		SrcMAC: net.HardwareAddr(append([]byte(nil), frame[6:12]...)),
	}

	etherType := binary.BigEndian.Uint16(frame[12:14])
	offset := 14

	// Optional 802.1Q VLAN tag.
	if etherType == ethernet.EtherTypeVLAN {
		if len(frame) < 18 {
			return nil, fmt.Errorf("%w: truncated VLAN", ErrFrameTooShort)
		}
		tci := binary.BigEndian.Uint16(frame[14:16])
		pdu.VLAN = &ethernet.VLANTag{
			Priority: uint8(tci >> 13),
			DEI:      (tci>>12)&1 == 1,
			VID:      tci & 0x0FFF,
		}
		etherType = binary.BigEndian.Uint16(frame[16:18])
		offset = 18
	}

	if etherType != EtherType {
		return nil, fmt.Errorf("%w: EtherType 0x%04X", ErrNotGOOSE, etherType)
	}

	// APDU header: AppID(2) + Length(2) + Reserved1(2) + Reserved2(2) = 8 bytes.
	if len(frame) < offset+8 {
		return nil, fmt.Errorf("%w: APDU header", ErrFrameTooShort)
	}
	pdu.AppID = binary.BigEndian.Uint16(frame[offset : offset+2])
	apduLen := int(binary.BigEndian.Uint16(frame[offset+2 : offset+4]))
	reserved1 := binary.BigEndian.Uint16(frame[offset+4 : offset+6])
	pdu.Simulation = reserved1&0x8000 != 0 // S bit (IEC 61850 Ed2.1 / 90-5)
	offset += 8

	if apduLen < 8 {
		return nil, fmt.Errorf("%w: APDU length %d", ErrBadPDU, apduLen)
	}
	payloadLen := apduLen - 8
	if payloadLen > len(frame)-offset {
		return nil, fmt.Errorf("%w: APDU length %d exceeds available frame data", ErrBadPDU, apduLen)
	}
	// Ethernet padding after the declared APDU is allowed, but every byte
	// declared as part of the APDU must be present in the captured frame.
	pduEnd := offset + payloadLen
	if pduEnd == offset {
		return nil, fmt.Errorf("%w: empty APDU payload", ErrBadPDU)
	}

	// Expect the APPLICATION 1 CONSTRUCTED tag (0x61), goosePDU.
	if frame[offset] != 0x61 {
		return nil, fmt.Errorf("%w: expected tag 0x61, got 0x%02X", ErrBadPDU, frame[offset])
	}
	offset++

	innerLen, newOff, err := berLength(frame, offset)
	if err != nil {
		return nil, fmt.Errorf("goose: pdu length: %w", err)
	}
	offset = newOff
	if offset > pduEnd {
		return nil, fmt.Errorf("%w: goosePDU length crosses APDU boundary", ErrBadPDU)
	}
	if innerLen > pduEnd-offset {
		return nil, fmt.Errorf("%w: goosePDU length %d exceeds APDU payload", ErrBadPDU, innerLen)
	}
	innerEnd := offset + innerLen
	if innerEnd != pduEnd {
		return nil, fmt.Errorf("%w: goosePDU length does not match APDU payload", ErrBadPDU)
	}

	// Iterate over goosePDU fields.
	var seenFields uint16
	for offset < innerEnd {
		tag := frame[offset]
		offset++

		fieldLen, nextOff, err := berLength(frame, offset)
		if err != nil {
			return nil, fmt.Errorf("goose: field length: %w", err)
		}
		offset = nextOff

		if offset > innerEnd || fieldLen > innerEnd-offset {
			return nil, fmt.Errorf("%w: field overflow at tag 0x%02X", ErrBadPDU, tag)
		}
		data := frame[offset : offset+fieldLen]
		field := gooseField(tag)
		if field != 0 {
			if seenFields&field != 0 {
				return nil, fmt.Errorf("%w: duplicate field 0x%02X", ErrBadPDU, tag)
			}
			seenFields |= field
		}

		switch tag {
		case 0x80: // gocbRef
			pdu.GocbRef = string(append([]byte(nil), data...))
		case 0x81: // timeAllowedToLive
			pdu.TimeAllowedToLiveMs, err = berUint32(data)
		case 0x82: // datSet
			pdu.DatSet = string(append([]byte(nil), data...))
		case 0x83: // goID
			pdu.GoID = string(append([]byte(nil), data...))
		case 0x84: // t (UtcTime)
			if len(data) != 8 {
				err = fmt.Errorf("UtcTime must contain 8 bytes, got %d", len(data))
			} else {
				pdu.Timestamp, pdu.TimeQuality = decodeUtcTime(data)
			}
		case 0x85: // stNum
			pdu.StNum, err = berUint32(data)
		case 0x86: // sqNum
			pdu.SqNum, err = berUint32(data)
		case 0x87: // test
			if len(data) != 1 {
				err = fmt.Errorf("test must contain 1 byte, got %d", len(data))
			} else {
				pdu.Test = data[0] != 0
			}
		case 0x88: // confRev
			pdu.ConfRev, err = berUint32(data)
		case 0x89: // ndsCom
			if len(data) != 1 {
				err = fmt.Errorf("ndsCom must contain 1 byte, got %d", len(data))
			} else {
				pdu.NdsCom = data[0] != 0
			}
		case 0x8A: // numDatSetEntries
			pdu.NumDatSetEntries, err = berUint32(data)
		case 0xAB: // allData [11] IMPLICIT SEQUENCE OF Data
			values, err := decodeDataSeq(data)
			if err != nil {
				return nil, fmt.Errorf("goose: allData: %w", err)
			}
			pdu.AllData = values
		}
		if err != nil {
			return nil, fmt.Errorf("%w: field 0x%02X: %v", ErrBadBER, tag, err)
		}
		offset += fieldLen
	}

	if err := validateGOOSEFields(pdu, seenFields); err != nil {
		return nil, err
	}
	return pdu, nil
}

// ── BER helpers ──────────────────────────────────────────────────────────────

func gooseField(tag byte) uint16 {
	if tag >= 0x80 && tag <= 0x8A {
		return uint16(1) << (tag - 0x80)
	}
	if tag == 0xAB {
		return fieldAllData
	}
	return 0
}

func validateGOOSEFields(pdu *PDU, seenFields uint16) error {
	if missing := requiredGOOSEFields &^ seenFields; missing != 0 {
		for _, tag := range requiredGOOSEFieldTags {
			if missing&gooseField(tag) != 0 {
				return fmt.Errorf("%w: missing required field 0x%02X", ErrBadPDU, tag)
			}
		}
	}
	if uint64(pdu.NumDatSetEntries) != uint64(len(pdu.AllData)) {
		return fmt.Errorf(
			"%w: numDatSetEntries=%d does not match allData entries=%d",
			ErrBadPDU,
			pdu.NumDatSetEntries,
			len(pdu.AllData),
		)
	}
	return nil
}

// berLength decodes a BER length and returns (length, new offset, error).
func berLength(data []byte, offset int) (int, int, error) {
	if offset >= len(data) {
		return 0, offset, fmt.Errorf("%w: length EOF", ErrBadBER)
	}
	b := data[offset]
	offset++
	if b < 0x80 {
		return int(b), offset, nil
	}
	n := int(b & 0x7F)
	if n == 0 || n > 4 || offset+n > len(data) {
		return 0, offset, fmt.Errorf("%w: bad long-form length", ErrBadBER)
	}
	l := 0
	for i := 0; i < n; i++ {
		l = (l << 8) | int(data[offset])
		offset++
	}
	return l, offset, nil
}

func berUint32(data []byte) (uint32, error) {
	value, err := decodeBERUnsigned(data, 4)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func berUint64(data []byte) (uint64, error) {
	return decodeBERUnsigned(data, 8)
}

// decodeBERUnsigned decodes a non-negative ASN.1 INTEGER. Values whose most
// significant magnitude bit is one must carry the sign-protecting 0x00 octet.
func decodeBERUnsigned(data []byte, magnitudeBytes int) (uint64, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("empty unsigned integer")
	}
	if len(data) > magnitudeBytes+1 {
		return 0, fmt.Errorf("unsigned integer exceeds %d bits", magnitudeBytes*8)
	}
	if len(data) == magnitudeBytes+1 {
		if data[0] != 0 || data[1]&0x80 == 0 {
			return 0, fmt.Errorf("unsigned integer exceeds %d bits", magnitudeBytes*8)
		}
		data = data[1:]
	} else {
		if data[0]&0x80 != 0 {
			return 0, fmt.Errorf("unsigned integer is encoded as negative")
		}
		if len(data) > 1 && data[0] == 0 && data[1]&0x80 == 0 {
			return 0, fmt.Errorf("non-minimal unsigned integer encoding")
		}
		if data[0] == 0 && len(data) > 1 {
			data = data[1:]
		}
	}
	var value uint64
	for _, b := range data {
		value = value<<8 | uint64(b)
	}
	return value, nil
}

// berInt decodes a variable-length BER-encoded signed integer.
func berInt(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	// Sign-extend the value.
	var v int64
	if data[0]&0x80 != 0 {
		v = -1
	}
	for _, b := range data {
		v = (v << 8) | int64(b)
	}
	return v
}

// decodeUtcTime parses an 8-byte IEC 61850 UtcTime:
//
//	[0..3] seconds since the Unix epoch (uint32 BE)
//	[4..6] fractional second (24 bits, fraction/2^24)
//	[7]    TimeQuality (IEC 61850-7-2):
//	         bit7=LeapSecondsKnown, bit6=ClockFailure,
//	         bit5=ClockNotSynchronized, bits4..0=Accuracy.
func decodeUtcTime(data []byte) (time.Time, TimeQuality) {
	if len(data) < 8 {
		return time.Time{}, TimeQuality{Accuracy: AccuracyUnspecified}
	}
	secs := binary.BigEndian.Uint32(data[0:4])
	frac := uint32(data[4])<<16 | uint32(data[5])<<8 | uint32(data[6])
	nsec := int64(frac) * 1_000_000_000 / (1 << 24)
	q := TimeQuality{
		LeapSecondsKnown:     data[7]&0x80 != 0,
		ClockFailure:         data[7]&0x40 != 0,
		ClockNotSynchronized: data[7]&0x20 != 0,
		Accuracy:             data[7] & 0x1F,
	}
	return time.Unix(int64(secs), nsec).UTC(), q
}

// decodeFloat parses an MMS FloatingPoint (IEC 61850-8-1):
//
//	[0]      exponent width in bits (8 for single, 11 for double)
//	[1..4]   IEEE 754 single (when expWidth=8)
//	[1..8]   IEEE 754 double (when expWidth=11)
func decodeFloat(data []byte) float64 {
	if len(data) < 5 {
		return 0
	}
	expWidth := data[0]
	switch {
	case expWidth == 8 && len(data) >= 5:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data[1:5])))
	case expWidth == 11 && len(data) >= 9:
		return math.Float64frombits(binary.BigEndian.Uint64(data[1:9]))
	}
	return 0
}

// ── Data tree decoder ────────────────────────────────────────────────────────

// decodeDataSeq parses a sequence of Data CHOICE elements.
// IEC 61850-8-1 §8.2.3.2: context-specific tags correspond to DataType values.
func decodeDataSeq(data []byte) ([]DataValue, error) {
	var out []DataValue
	offset := 0
	for offset < len(data) {
		tag := data[offset]
		offset++

		fieldLen, nextOff, err := berLength(data, offset)
		if err != nil {
			return nil, err
		}
		offset = nextOff
		if offset+fieldLen > len(data) {
			return nil, fmt.Errorf("%w: data element overflow", ErrBadBER)
		}
		val := data[offset : offset+fieldLen]
		offset += fieldLen

		dv, err := decodeDataValue(tag, val)
		if err != nil {
			return nil, err
		}
		out = append(out, dv)
	}
	return out, nil
}

// decodeDataValue parses one Data CHOICE element by its context-specific tag.
// Context-specific bits: class=10 (context), P/C bit for CONSTRUCTED,
// number = DataType.
func decodeDataValue(tag byte, val []byte) (DataValue, error) {
	// class=10 context-specific: the top two bits are 10.
	// CONSTRUCTED: bit 5 (0x20).
	// number = tag & 0x1F
	number := tag & 0x1F
	constructed := tag&0x20 != 0

	dv := DataValue{Type: DataType(number)}

	switch DataType(number) {
	case DataTypeArray, DataTypeStructure:
		if !constructed {
			return dv, fmt.Errorf("%w: array/struct must be constructed", ErrBadBER)
		}
		children, err := decodeDataSeq(val)
		if err != nil {
			return dv, err
		}
		dv.Children = children
	case DataTypeBoolean:
		if len(val) > 0 {
			dv.Bool = val[0] != 0
		}
	case DataTypeBitString, DataTypeBooleanArray:
		if len(val) == 0 {
			return dv, fmt.Errorf("%w: BIT STRING has no unused-bits octet", ErrBadBER)
		}
		unused := int(val[0])
		if unused > 7 || (len(val) == 1 && unused != 0) {
			return dv, fmt.Errorf("%w: invalid BIT STRING unused-bits value %d", ErrBadBER, unused)
		}
		payload := val[1:]
		if unused > 0 && payload[len(payload)-1]&byte((1<<unused)-1) != 0 {
			return dv, fmt.Errorf("%w: non-zero BIT STRING padding bits", ErrBadBER)
		}
		dv.Bytes = append([]byte(nil), payload...)
		dv.BitLength = len(payload)*8 - unused
	case DataTypeOctetString:
		dv.Bytes = append([]byte(nil), val...)
	case DataTypeInteger, DataTypeBCD:
		if len(val) > 8 {
			return dv, fmt.Errorf("%w: signed integer exceeds 64 bits", ErrBadBER)
		}
		dv.Int = berInt(val)
	case DataTypeUnsigned:
		value, err := berUint64(val)
		if err != nil {
			return dv, fmt.Errorf("%w: %v", ErrBadBER, err)
		}
		dv.UInt = value
	case DataTypeFloatingPoint, DataTypeReal:
		dv.Float = decodeFloat(val)
	case DataTypeVisibleString:
		dv.String = string(append([]byte(nil), val...))
	case DataTypeUTCTime:
		dv.Time, dv.TimeQuality = decodeUtcTime(val)
	case DataTypeBinaryTime:
		dv.Bytes = append([]byte(nil), val...)
	default:
		// Preserve raw Bytes for diagnostics when the tag is unknown.
		dv.Type = DataTypeUnknown
		dv.Bytes = append([]byte(nil), val...)
	}
	return dv, nil
}
