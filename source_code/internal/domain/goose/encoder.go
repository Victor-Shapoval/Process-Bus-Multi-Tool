package goose

import (
	"encoding/binary"
	"math"
	"time"

	"pbmt/internal/domain/ethernet"
)

// Encode serializes a PDU into a raw Ethernet frame, including the L2 header.
// It is a pure function with no I/O.
func Encode(pdu *PDU) []byte {
	// 1. Encode goosePDU (the body inside tag 0x61).
	inner := encodeGoosePDUFields(pdu)
	goosePDU := berWrap(0x61, inner) // APPLICATION 1 CONSTRUCTED

	// 2. APDU: AppID(2) + Length(2) + Reserved1(2) + Reserved2(2) + goosePDU
	apduPayload := goosePDU
	apduLen := 8 + len(apduPayload) // 8 = AppID + Length + Res1 + Res2

	apdu := make([]byte, apduLen)
	binary.BigEndian.PutUint16(apdu[0:2], pdu.AppID)
	binary.BigEndian.PutUint16(apdu[2:4], uint16(apduLen))
	var reserved1 uint16
	if pdu.Simulation {
		reserved1 |= 0x8000
	}
	binary.BigEndian.PutUint16(apdu[4:6], reserved1)
	binary.BigEndian.PutUint16(apdu[6:8], 0) // Reserved2
	copy(apdu[8:], apduPayload)

	// 3. Ethernet header
	var frame []byte
	if pdu.VLAN != nil {
		// with 802.1Q VLAN tag
		frame = make([]byte, 18+len(apdu))
		copy(frame[0:6], pdu.DstMAC)
		copy(frame[6:12], pdu.SrcMAC)
		binary.BigEndian.PutUint16(frame[12:14], ethernet.EtherTypeVLAN)
		tci := uint16(pdu.VLAN.Priority)<<13 | pdu.VLAN.VID&0x0FFF
		if pdu.VLAN.DEI {
			tci |= 1 << 12
		}
		binary.BigEndian.PutUint16(frame[14:16], tci)
		binary.BigEndian.PutUint16(frame[16:18], EtherType)
		copy(frame[18:], apdu)
	} else {
		frame = make([]byte, 14+len(apdu))
		copy(frame[0:6], pdu.DstMAC)
		copy(frame[6:12], pdu.SrcMAC)
		binary.BigEndian.PutUint16(frame[12:14], EtherType)
		copy(frame[14:], apdu)
	}

	return frame
}

// encodeGoosePDUFields encodes all goosePDU fields inside tag 0x61.
func encodeGoosePDUFields(pdu *PDU) []byte {
	var buf []byte

	// gocbRef [0x80]
	buf = append(buf, berTLV(0x80, []byte(pdu.GocbRef))...)
	// timeAllowedToLive [0x81]
	buf = append(buf, berTLV(0x81, berEncodeUint(pdu.TimeAllowedToLiveMs))...)
	// datSet [0x82]
	buf = append(buf, berTLV(0x82, []byte(pdu.DatSet))...)
	// goID [0x83]
	buf = append(buf, berTLV(0x83, []byte(pdu.GoID))...)
	// t [0x84] UtcTime (8 bytes)
	buf = append(buf, berTLV(0x84, encodeUtcTime(pdu.Timestamp, pdu.TimeQuality))...)
	// stNum [0x85]
	buf = append(buf, berTLV(0x85, berEncodeUint(pdu.StNum))...)
	// sqNum [0x86]
	buf = append(buf, berTLV(0x86, berEncodeUint(pdu.SqNum))...)
	// test [0x87]
	buf = append(buf, berTLV(0x87, berEncodeBool(pdu.Test))...)
	// confRev [0x88]
	buf = append(buf, berTLV(0x88, berEncodeUint(pdu.ConfRev))...)
	// ndsCom [0x89]
	buf = append(buf, berTLV(0x89, berEncodeBool(pdu.NdsCom))...)
	// numDatSetEntries [0x8A]
	buf = append(buf, berTLV(0x8A, berEncodeUint(pdu.NumDatSetEntries))...)
	// allData [0xAB] IMPLICIT SEQUENCE OF Data
	buf = append(buf, berTLV(0xAB, encodeDataSeq(pdu.AllData))...)

	return buf
}

// encodeUtcTime encodes an 8-byte IEC 61850 UtcTime.
func encodeUtcTime(t time.Time, q TimeQuality) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], uint32(t.Unix()))
	frac := uint32(int64(t.Nanosecond()) * (1 << 24) / 1_000_000_000)
	buf[4] = byte(frac >> 16)
	buf[5] = byte(frac >> 8)
	buf[6] = byte(frac)
	var qb byte
	if q.LeapSecondsKnown {
		qb |= 0x80
	}
	if q.ClockFailure {
		qb |= 0x40
	}
	if q.ClockNotSynchronized {
		qb |= 0x20
	}
	qb |= q.Accuracy & 0x1F
	buf[7] = qb
	return buf
}

// encodeDataSeq encodes a sequence of DataValue elements.
func encodeDataSeq(values []DataValue) []byte {
	var buf []byte
	for _, dv := range values {
		buf = append(buf, encodeDataValue(dv)...)
	}
	return buf
}

// encodeDataValue encodes one DataValue.
func encodeDataValue(dv DataValue) []byte {
	tag := byte(dv.Type) | 0x80 // context-specific class

	switch dv.Type {
	case DataTypeArray, DataTypeStructure:
		tag |= 0x20 // constructed
		return berTLV(tag, encodeDataSeq(dv.Children))
	case DataTypeBoolean:
		return berTLV(tag, berEncodeBool(dv.Bool))
	case DataTypeBitString, DataTypeBooleanArray:
		return berTLV(tag, encodeBitString(dv))
	case DataTypeOctetString, DataTypeBinaryTime:
		return berTLV(tag, dv.Bytes)
	case DataTypeInteger, DataTypeBCD:
		return berTLV(tag, berEncodeInt(dv.Int))
	case DataTypeUnsigned:
		return berTLV(tag, berEncodeUint64(dv.UInt))
	case DataTypeFloatingPoint, DataTypeReal:
		return berTLV(tag, encodeFloat(dv.Float))
	case DataTypeVisibleString:
		return berTLV(tag, []byte(dv.String))
	case DataTypeUTCTime:
		return berTLV(tag, encodeUtcTime(dv.Time, dv.TimeQuality))
	default:
		return berTLV(tag, dv.Bytes)
	}
}

// encodeBitString returns the BER contents octets of a BIT STRING. The first
// octet is the number of unused bits in the final payload octet (X.690 §8.6).
func encodeBitString(dv DataValue) []byte {
	bitLength := dv.BitLength
	if bitLength <= 0 || bitLength > len(dv.Bytes)*8 {
		bitLength = len(dv.Bytes) * 8
	}
	payloadLen := (bitLength + 7) / 8
	unused := payloadLen*8 - bitLength
	out := make([]byte, 1+payloadLen)
	out[0] = byte(unused)
	copy(out[1:], dv.Bytes[:payloadLen])
	if payloadLen > 0 && unused > 0 {
		out[len(out)-1] &= byte(0xFF << unused)
	}
	return out
}

// ── BER encoding helpers ────────────────────────────────────────────────────

// berTLV creates a TLV: tag + length + value.
func berTLV(tag byte, value []byte) []byte {
	l := len(value)
	var out []byte
	out = append(out, tag)
	out = append(out, berEncodeLength(l)...)
	out = append(out, value...)
	return out
}

// berWrap wraps data in a TLV with the specified tag.
func berWrap(tag byte, data []byte) []byte {
	return berTLV(tag, data)
}

// berEncodeLength encodes a BER length.
func berEncodeLength(l int) []byte {
	if l < 0x80 {
		return []byte{byte(l)}
	}
	if l <= 0xFF {
		return []byte{0x81, byte(l)}
	}
	if l <= 0xFFFF {
		return []byte{0x82, byte(l >> 8), byte(l)}
	}
	return []byte{0x83, byte(l >> 16), byte(l >> 8), byte(l)}
}

// berEncodeUint encodes an unsigned integer using the minimum number of bytes.
func berEncodeUint(v uint32) []byte {
	return berEncodeUint64(uint64(v))
}

func berEncodeUint64(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	start := 0
	for start < len(buf)-1 && buf[start] == 0 {
		start++
	}
	magnitude := buf[start:]
	if magnitude[0]&0x80 != 0 {
		out := make([]byte, len(magnitude)+1)
		copy(out[1:], magnitude)
		return out
	}
	return append([]byte(nil), magnitude...)
}

// berEncodeInt encodes a signed integer using the minimum number of bytes.
func berEncodeInt(v int64) []byte {
	if v == 0 {
		return []byte{0}
	}
	// Find the minimum number of bytes.
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	// Skip leading 0x00 bytes for positive values or 0xFF bytes for negative values.
	start := 0
	if v > 0 {
		for start < 7 && buf[start] == 0 {
			start++
		}
		// A leading 0x00 is required for the sign when the high bit is 1.
		if buf[start]&0x80 != 0 {
			start--
		}
	} else {
		for start < 7 && buf[start] == 0xFF && buf[start+1]&0x80 != 0 {
			start++
		}
	}
	return append([]byte(nil), buf[start:]...)
}

// berEncodeBool encodes a BOOLEAN (one byte: 0x00 or 0xFF).
func berEncodeBool(b bool) []byte {
	if b {
		return []byte{0xFF}
	}
	return []byte{0x00}
}

// encodeFloat encodes an MMS FloatingPoint (exponent width + IEEE 754 single).
func encodeFloat(f float64) []byte {
	buf := make([]byte, 5)
	buf[0] = 8 // exponent width = 8 (single precision)
	binary.BigEndian.PutUint32(buf[1:5], math.Float32bits(float32(f)))
	return buf
}
