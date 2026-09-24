package sv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"pbmt/internal/domain/ethernet"
)

// Decoding errors.
var (
	ErrFrameTooShort = errors.New("sv: frame too short")
	ErrNotSV         = errors.New("sv: not an SV frame")
	ErrBadBER        = errors.New("sv: invalid BER encoding")
	ErrBadAPDU       = errors.New("sv: malformed APDU")
	ErrBadASDU       = errors.New("sv: malformed ASDU")
	ErrInvalidLive   = errors.New("sv: frame is invalid for live processing")
)

// Decode parses a raw Ethernet frame and extracts SV ASDUs.
// In 9-2LE, a frame usually contains one ASDU (noASDU=1).
// Malformed required live fields are reported as ErrInvalidLive;
// use DecodeFrame for tolerant offline analysis.
// The function is pure: it performs no I/O and uses no global state.
func Decode(frame []byte) ([]ASDU, error) {
	decoded, err := DecodeFrame(frame)
	if err != nil {
		return nil, err
	}
	if decoded.NonSV {
		return nil, fmt.Errorf("%w: EtherType 0x%04X", ErrNotSV, decoded.EtherType)
	}
	if len(decoded.ASDUs) == 0 {
		return nil, fmt.Errorf("%w: frame contains no ASDU", ErrInvalidLive)
	}
	for _, warning := range decoded.Warnings {
		if warning.RejectLive {
			return nil, fmt.Errorf("%w: %s", ErrInvalidLive, warning.Message)
		}
	}
	return decoded.ASDUs, nil
}

// DecodeFrame returns an extended decoding result for offline analysis.
// Non-SV frames are not errors, allowing the caller to count them statistically.
func DecodeFrame(frame []byte) (DecodedFrame, error) {
	var out DecodedFrame
	if len(frame) < 14 {
		return out, fmt.Errorf("%w: %d bytes", ErrFrameTooShort, len(frame))
	}

	dstMAC := net.HardwareAddr(append([]byte(nil), frame[0:6]...))
	srcMAC := net.HardwareAddr(append([]byte(nil), frame[6:12]...))
	etherType := binary.BigEndian.Uint16(frame[12:14])
	offset := 14

	var vlan *ethernet.VLANTag
	if etherType == ethernet.EtherTypeVLAN {
		if len(frame) < 18 {
			return out, fmt.Errorf("%w: truncated VLAN", ErrFrameTooShort)
		}
		tci := binary.BigEndian.Uint16(frame[14:16])
		vlan = &ethernet.VLANTag{
			Priority: uint8(tci >> 13),
			DEI:      (tci>>12)&1 == 1,
			VID:      tci & 0x0FFF,
		}
		etherType = binary.BigEndian.Uint16(frame[16:18])
		offset = 18
	}
	out.EtherType = etherType
	if etherType != EtherType {
		out.NonSV = true
		return out, nil
	}

	if len(frame) < offset+8 {
		return out, fmt.Errorf("%w: APDU header", ErrFrameTooShort)
	}
	appID := binary.BigEndian.Uint16(frame[offset : offset+2])
	apduLen := int(binary.BigEndian.Uint16(frame[offset+2 : offset+4]))
	reserved1 := binary.BigEndian.Uint16(frame[offset+4 : offset+6])
	reserved2 := binary.BigEndian.Uint16(frame[offset+6 : offset+8])
	simulation := reserved1&0x8000 != 0
	offset += 8

	pduEnd := offset + apduLen - 8
	if apduLen < 8 {
		return out, fmt.Errorf("%w: APDU length %d", ErrBadAPDU, apduLen)
	}
	if pduEnd > len(frame) {
		out.Warnings = append(out.Warnings, liveFrameWarning(fmt.Sprintf("APDU length %d exceeds remaining frame payload %d", apduLen, len(frame)-(offset-8))))
		pduEnd = len(frame)
	}
	if pduEnd <= offset {
		return out, fmt.Errorf("%w: empty APDU payload", ErrBadAPDU)
	}

	if frame[offset] != 0x60 {
		return out, fmt.Errorf("%w: expected savPDU tag 0x60, got 0x%02X", ErrBadAPDU, frame[offset])
	}
	offset++
	savLen, newOffset, err := berLength(frame, offset)
	if err != nil {
		return out, err
	}
	offset = newOffset
	savEnd := offset + savLen
	if savEnd > pduEnd {
		out.Warnings = append(out.Warnings, liveFrameWarning("savPDU length exceeds APDU payload"))
		savEnd = pduEnd
	}

	noASDU := 1
	if offset < savEnd && frame[offset] == 0x80 {
		offset++
		fieldLen, nOff, err := berLength(frame, offset)
		if err != nil {
			return out, err
		}
		offset = nOff
		if offset+fieldLen > savEnd {
			return out, fmt.Errorf("%w: noASDU overflow", ErrBadAPDU)
		}
		if fieldLen > 0 {
			noASDU = int(berUint(frame[offset : offset+fieldLen]))
		}
		offset += fieldLen
	} else {
		out.Warnings = append(out.Warnings, liveFrameWarning("missing noASDU field, assuming 1"))
	}
	out.DeclaredASDU = noASDU

	if offset >= savEnd || frame[offset] != 0xA2 {
		return out, fmt.Errorf("%w: expected seqASDU tag 0xA2", ErrBadAPDU)
	}
	offset++
	seqLen, nOff, err := berLength(frame, offset)
	if err != nil {
		return out, err
	}
	offset = nOff
	seqEnd := offset + seqLen
	if seqEnd > savEnd {
		out.Warnings = append(out.Warnings, liveFrameWarning("seqASDU length exceeds savPDU payload"))
		seqEnd = savEnd
	}

	for offset < seqEnd {
		if frame[offset] != 0x30 {
			return out, fmt.Errorf("%w: expected ASDU tag 0x30, got 0x%02X", ErrBadASDU, frame[offset])
		}
		offset++
		asduLen, nOff, err := berLength(frame, offset)
		if err != nil {
			return out, err
		}
		offset = nOff
		asduEnd := offset + asduLen
		if asduEnd > seqEnd {
			out.Warnings = append(out.Warnings, liveFrameWarning("ASDU length exceeds seqASDU payload"))
			asduEnd = seqEnd
		}

		asdu, warnings, err := decodeASDU(frame, offset, asduEnd)
		if err != nil {
			return out, err
		}
		asdu.DstMAC = dstMAC
		asdu.SrcMAC = srcMAC
		asdu.VLAN = vlan
		asdu.AppID = appID
		asdu.APDULength = apduLen
		asdu.Reserved1 = reserved1
		asdu.Reserved2 = reserved2
		asdu.Simulation = simulation
		asduIndex := len(out.ASDUs)
		for i := range warnings {
			warnings[i].ASDUScoped = true
			warnings[i].ASDUIndex = asduIndex
		}
		out.ASDUs = append(out.ASDUs, asdu)
		out.Warnings = append(out.Warnings, warnings...)
		offset = asduEnd
	}
	out.ActualASDUs = len(out.ASDUs)
	if noASDU != len(out.ASDUs) {
		out.Warnings = append(out.Warnings, liveFrameWarning(fmt.Sprintf("noASDU=%d but decoded %d ASDU", noASDU, len(out.ASDUs))))
	}
	return out, nil
}

func decodeASDU(frame []byte, offset, end int) (ASDU, []DecodeWarning, error) {
	var a ASDU
	var warnings []DecodeWarning
	for offset < end {
		tag := frame[offset]
		offset++
		fieldLen, nOff, err := berLength(frame, offset)
		if err != nil {
			return a, warnings, err
		}
		offset = nOff
		if offset+fieldLen > end {
			return a, warnings, fmt.Errorf("%w: field overflow at tag 0x%02X", ErrBadASDU, tag)
		}
		data := frame[offset : offset+fieldLen]

		switch tag {
		case 0x80:
			a.SvID = string(append([]byte(nil), data...))
			a.HasSvID = true
			if len(data) == 0 {
				warnings = append(warnings, liveASDUWarning("mandatory svID is empty"))
			}
		case 0x81:
			a.DatSet = string(append([]byte(nil), data...))
		case 0x82:
			if len(data) != 2 {
				warnings = append(warnings, liveASDUWarning(fmt.Sprintf("smpCnt length %d, expected 2", len(data))))
			}
			a.SmpCnt = uint16(berUint(data))
			a.HasSmpCnt = true
		case 0x83:
			if len(data) != 4 {
				return a, warnings, fmt.Errorf("%w: confRev length %d, expected 4", ErrBadASDU, len(data))
			}
			a.ConfRev = binary.BigEndian.Uint32(data)
			a.HasConfRev = true
		case 0x84:
			if len(data) != 8 {
				warnings = append(warnings, liveASDUWarning(fmt.Sprintf("refrTm length %d, expected 8", len(data))))
			}
			a.RefrTm, a.RefrTmQ = decodeUtcTime(data)
			a.HasRefrTm = true
		case 0x85:
			if len(data) != 1 {
				warnings = append(warnings, liveASDUWarning(fmt.Sprintf("smpSynch length %d, expected 1", len(data))))
			}
			if len(data) > 0 {
				a.SmpSynch = SmpSynch(data[0])
				a.HasSmpSynch = true
				if a.SmpSynch > SmpSynchGlobal {
					warnings = append(warnings, liveASDUWarning(fmt.Sprintf("invalid smpSynch value %d", data[0])))
				}
			}
		case 0x86:
			if len(data) != 2 {
				warnings = append(warnings, liveASDUWarning(fmt.Sprintf("smpRate length %d, expected 2", len(data))))
			}
			a.SmpRate = uint16(berUint(data))
			a.HasSmpRate = true
		case 0x87:
			a.HasSeqData = true
			a.SeqData = append([]byte(nil), data...)
			if len(data) != SeqDataLen {
				warnings = append(warnings, liveASDUWarning(fmt.Sprintf("seqData length %d, expected exactly %d for 9-2LE", len(data), SeqDataLen)))
			}
			if len(data) >= SeqDataLen {
				for ch := 0; ch < NumChannels; ch++ {
					off := ch * 8
					a.Channels[ch] = int32(binary.BigEndian.Uint32(data[off : off+4]))
					a.Quality[ch] = Quality(binary.BigEndian.Uint32(data[off+4 : off+8]))
				}
			}
		case 0x88:
			if len(data) != 2 {
				return a, warnings, fmt.Errorf("%w: smpMod length %d, expected 2", ErrBadASDU, len(data))
			}
			smpMod := binary.BigEndian.Uint16(data)
			if smpMod > uint16(SmpModSecondsPerSample) {
				return a, warnings, fmt.Errorf("%w: invalid smpMod value %d", ErrBadASDU, smpMod)
			}
			a.SmpMod = SmpMod(smpMod)
			a.HasSmpMod = true
		default:
			warnings = append(warnings, DecodeWarning{Message: fmt.Sprintf("unknown ASDU tag 0x%02X length %d", tag, fieldLen)})
		}
		offset += fieldLen
	}
	if !a.HasSvID {
		warnings = append(warnings, liveASDUWarning("missing mandatory svID"))
	}
	if !a.HasSmpCnt {
		warnings = append(warnings, liveASDUWarning("missing mandatory smpCnt"))
	}
	if !a.HasConfRev {
		warnings = append(warnings, liveASDUWarning("missing mandatory confRev"))
	}
	if !a.HasSmpSynch {
		warnings = append(warnings, liveASDUWarning("missing mandatory smpSynch"))
	}
	if !a.HasSeqData {
		warnings = append(warnings, liveASDUWarning("missing mandatory seqData"))
	}
	return a, warnings, nil
}

func liveFrameWarning(message string) DecodeWarning {
	return DecodeWarning{Message: message, RejectLive: true}
}

func liveASDUWarning(message string) DecodeWarning {
	return DecodeWarning{Message: message, RejectLive: true}
}

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

func berUint(data []byte) uint32 {
	var v uint32
	for _, b := range data {
		v = (v << 8) | uint32(b)
	}
	return v
}

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
