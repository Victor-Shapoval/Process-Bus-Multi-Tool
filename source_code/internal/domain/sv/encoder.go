package sv

import (
	"encoding/binary"
	"time"

	"pbmt/internal/domain/ethernet"
)

// Encode serializes one or more ASDUs into an IEC 61850-9-2LE Ethernet frame.
// All ASDUs share the Ethernet/APDU fields of the first ASDU. The exact output
// size is calculated up front, so the hot publisher path needs one allocation.
func Encode(asdus []ASDU) []byte {
	if len(asdus) == 0 {
		return nil
	}
	first := &asdus[0]

	seqASDULen := 0
	for i := range asdus {
		seqASDULen += encodedASDULen(&asdus[i])
	}
	noASDUValueLen := berUintSize(uint32(len(asdus)))
	savInnerLen := berTLVSize(noASDUValueLen) + berTLVSize(seqASDULen)
	savPDULen := berTLVSize(savInnerLen)
	estimatedAPDULen := 8 + savPDULen

	headerLen := 14
	if first.VLAN != nil {
		headerLen = 18
	}
	// Reserve the calculated size, but derive the protocol Length field from the
	// bytes that were actually serialized below. This prevents a future optional
	// field from silently making the APDU header disagree with the wire payload.
	frame := make([]byte, headerLen+8, headerLen+estimatedAPDULen)
	writeEthernetHeader(frame, first)

	apdu := frame[headerLen:]
	binary.BigEndian.PutUint16(apdu[0:2], first.AppID)
	if first.Simulation {
		binary.BigEndian.PutUint16(apdu[4:6], 0x8000)
	}
	// Reserved1 without simulation and Reserved2 are already zeroed.

	frame = appendBERHeader(frame, 0x60, savInnerLen)
	frame = appendTLVUint(frame, 0x80, uint32(len(asdus)))
	frame = appendBERHeader(frame, 0xA2, seqASDULen)
	for i := range asdus {
		frame = appendASDU(frame, &asdus[i])
	}

	apduLen := len(frame) - headerLen
	if apduLen > int(^uint16(0)) {
		return nil
	}
	binary.BigEndian.PutUint16(frame[headerLen+2:headerLen+4], uint16(apduLen))
	return frame
}

func writeEthernetHeader(frame []byte, first *ASDU) {
	copy(frame[0:6], first.DstMAC)
	copy(frame[6:12], first.SrcMAC)
	if first.VLAN == nil {
		binary.BigEndian.PutUint16(frame[12:14], EtherType)
		return
	}
	binary.BigEndian.PutUint16(frame[12:14], ethernet.EtherTypeVLAN)
	tci := uint16(first.VLAN.Priority)<<13 | first.VLAN.VID&0x0FFF
	if first.VLAN.DEI {
		tci |= 1 << 12
	}
	binary.BigEndian.PutUint16(frame[14:16], tci)
	binary.BigEndian.PutUint16(frame[16:18], EtherType)
}

func encodedASDULen(a *ASDU) int {
	return berTLVSize(encodedASDUInnerLen(a))
}

func encodedASDUInnerLen(a *ASDU) int {
	n := berTLVSize(len(a.SvID))
	if a.DatSet != "" {
		n += berTLVSize(len(a.DatSet))
	}
	n += berTLVSize(2) // smpCnt
	n += berTLVSize(4) // confRev is INT32U and has a fixed four-octet value
	if !a.RefrTm.IsZero() {
		n += berTLVSize(8)
	}
	n += berTLVSize(1) // smpSynch
	if a.SmpRate > 0 {
		n += berTLVSize(2)
	}
	n += berTLVSize(seqDataLen)
	if a.SmpMod != SmpModSamplesPerPeriod {
		n += berTLVSize(2) // smpMod is INT16U
	}
	return n
}

func appendASDU(dst []byte, a *ASDU) []byte {
	dst = appendBERHeader(dst, 0x30, encodedASDUInnerLen(a))
	dst = appendTLVBytes(dst, 0x80, a.SvID)
	if a.DatSet != "" {
		dst = appendTLVBytes(dst, 0x81, a.DatSet)
	}
	dst = appendTLVUint16(dst, 0x82, a.SmpCnt)
	dst = appendBERHeader(dst, 0x83, 4)
	dst = append(dst, byte(a.ConfRev>>24), byte(a.ConfRev>>16), byte(a.ConfRev>>8), byte(a.ConfRev))
	if !a.RefrTm.IsZero() {
		dst = appendBERHeader(dst, 0x84, 8)
		dst = appendUtcTime(dst, a.RefrTm, a.RefrTmQ)
	}
	dst = appendBERHeader(dst, 0x85, 1)
	dst = append(dst, byte(a.SmpSynch))
	if a.SmpRate > 0 {
		dst = appendTLVUint16(dst, 0x86, a.SmpRate)
	}
	dst = appendBERHeader(dst, 0x87, seqDataLen)
	for ch := 0; ch < NumChannels; ch++ {
		var sample [8]byte
		binary.BigEndian.PutUint32(sample[0:4], uint32(a.Channels[ch]))
		binary.BigEndian.PutUint32(sample[4:8], uint32(a.Quality[ch]))
		dst = append(dst, sample[:]...)
	}
	if a.SmpMod != SmpModSamplesPerPeriod {
		dst = appendTLVUint16(dst, 0x88, uint16(a.SmpMod))
	}
	return dst
}

func appendTLVBytes(dst []byte, tag byte, value string) []byte {
	dst = appendBERHeader(dst, tag, len(value))
	return append(dst, value...)
}

func appendTLVUint16(dst []byte, tag byte, value uint16) []byte {
	dst = appendBERHeader(dst, tag, 2)
	return append(dst, byte(value>>8), byte(value))
}

func appendTLVUint(dst []byte, tag byte, value uint32) []byte {
	dst = appendBERHeader(dst, tag, berUintSize(value))
	switch {
	case value == 0:
		return append(dst, 0)
	case value <= 0xFF:
		return append(dst, byte(value))
	case value <= 0xFFFF:
		return append(dst, byte(value>>8), byte(value))
	case value <= 0xFFFFFF:
		return append(dst, byte(value>>16), byte(value>>8), byte(value))
	default:
		return append(dst, byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	}
}

func appendBERHeader(dst []byte, tag byte, length int) []byte {
	dst = append(dst, tag)
	return appendBERLength(dst, length)
}

func appendBERLength(dst []byte, length int) []byte {
	switch {
	case length < 0x80:
		return append(dst, byte(length))
	case length <= 0xFF:
		return append(dst, 0x81, byte(length))
	case length <= 0xFFFF:
		return append(dst, 0x82, byte(length>>8), byte(length))
	default:
		return append(dst, 0x83, byte(length>>16), byte(length>>8), byte(length))
	}
}

func berTLVSize(valueLen int) int {
	return 1 + berLengthSize(valueLen) + valueLen
}

func berLengthSize(length int) int {
	switch {
	case length < 0x80:
		return 1
	case length <= 0xFF:
		return 2
	case length <= 0xFFFF:
		return 3
	default:
		return 4
	}
}

func berUintSize(value uint32) int {
	switch {
	case value <= 0xFF:
		return 1
	case value <= 0xFFFF:
		return 2
	case value <= 0xFFFFFF:
		return 3
	default:
		return 4
	}
}

func appendUtcTime(dst []byte, t time.Time, q TimeQuality) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint32(encoded[0:4], uint32(t.Unix()))
	frac := uint32(int64(t.Nanosecond()) * (1 << 24) / 1_000_000_000)
	encoded[4] = byte(frac >> 16)
	encoded[5] = byte(frac >> 8)
	encoded[6] = byte(frac)
	if q.LeapSecondsKnown {
		encoded[7] |= 0x80
	}
	if q.ClockFailure {
		encoded[7] |= 0x40
	}
	if q.ClockNotSynchronized {
		encoded[7] |= 0x20
	}
	encoded[7] |= q.Accuracy & 0x1F
	return append(dst, encoded[:]...)
}
