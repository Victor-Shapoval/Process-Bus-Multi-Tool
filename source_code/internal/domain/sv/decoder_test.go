package sv

import (
	"encoding/binary"
	"errors"
	"testing"
)

func berTLV(tag byte, value []byte) []byte {
	out := make([]byte, 0, berTLVSize(len(value)))
	out = appendBERHeader(out, tag, len(value))
	return append(out, value...)
}

// buildTestFrame builds a minimal valid SV Ethernet frame without VLAN.
func buildTestFrame(appID uint16, svID string, smpCnt uint16, confRev uint32, channels [8]int32, qualities [8]Quality) []byte {
	// seqData: 8 channels × (int32 + uint32) = 64 bytes
	seqData := make([]byte, 64)
	for ch := 0; ch < 8; ch++ {
		off := ch * 8
		binary.BigEndian.PutUint32(seqData[off:], uint32(channels[ch]))
		binary.BigEndian.PutUint32(seqData[off+4:], uint32(qualities[ch]))
	}

	// ASDU fields
	asduPayload := berTLV(0x80, []byte(svID))                          // svID
	asduPayload = append(asduPayload, berTLV(0x82, u16be(smpCnt))...)  // smpCnt
	asduPayload = append(asduPayload, berTLV(0x83, u32be(confRev))...) // confRev
	asduPayload = append(asduPayload, berTLV(0x85, []byte{2})...)      // smpSynch=global
	asduPayload = append(asduPayload, berTLV(0x87, seqData)...)        // seqData

	return buildTestFrameFromASDUPayload(appID, asduPayload)
}

func buildTestFrameFromASDUPayload(appID uint16, asduPayload []byte) []byte {
	asdu := berTLV(0x30, asduPayload) // ASDU SEQUENCE
	seqASDU := berTLV(0xA2, asdu)     // seqASDU
	noASDU := berTLV(0x80, []byte{1}) // noASDU=1
	savPDU := berTLV(0x60, append(noASDU, seqASDU...))

	// APDU: AppID(2) + Length(2) + Reserved1(2) + Reserved2(2) + savPDU
	apduLen := 8 + len(savPDU)
	apdu := make([]byte, 8, 8+len(savPDU))
	binary.BigEndian.PutUint16(apdu[0:], appID)
	binary.BigEndian.PutUint16(apdu[2:], uint16(apduLen))
	// reserved1=0, reserved2=0
	apdu = append(apdu, savPDU...)

	// Ethernet: dst(6) + src(6) + EtherType(2) + APDU
	frame := make([]byte, 14+len(apdu))
	copy(frame[0:6], []byte{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01})  // dst
	copy(frame[6:12], []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}) // src
	binary.BigEndian.PutUint16(frame[12:], EtherType)
	copy(frame[14:], apdu)
	return frame
}

// berTLV now lives in encoder.go; tests use it directly.

func u16be(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32be(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func TestDecodeBasic(t *testing.T) {
	var ch [8]int32
	ch[ChIa] = 1_000_000  // 1000 A
	ch[ChUa] = 13_279_000 // 132790 V
	var q [8]Quality

	frame := buildTestFrame(0x4001, "MU0001", 42, 1, ch, q)
	asdus, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(asdus) != 1 {
		t.Fatalf("expected 1 ASDU, got %d", len(asdus))
	}
	a := asdus[0]
	if a.AppID != 0x4001 {
		t.Errorf("AppID = 0x%04X, want 0x4001", a.AppID)
	}
	if a.SvID != "MU0001" {
		t.Errorf("SvID = %q, want MU0001", a.SvID)
	}
	if a.SmpCnt != 42 {
		t.Errorf("SmpCnt = %d, want 42", a.SmpCnt)
	}
	if a.ConfRev != 1 {
		t.Errorf("ConfRev = %d, want 1", a.ConfRev)
	}
	if a.SmpSynch != SmpSynchGlobal {
		t.Errorf("SmpSynch = %v, want global", a.SmpSynch)
	}
	if a.Channels[ChIa] != 1_000_000 {
		t.Errorf("Ia raw = %d, want 1000000", a.Channels[ChIa])
	}
	if got := a.PhysicalValue(ChIa); got != 1000.0 {
		t.Errorf("Ia = %g A, want 1000", got)
	}
	if got := a.PhysicalValue(ChUa); got != 132790.0 {
		t.Errorf("Ua = %g V, want 132790", got)
	}
}

func TestDecodeOptionalFields(t *testing.T) {
	// Build an ASDU with datSet, refrTm, smpRate, and smpMod.
	seqData := make([]byte, 64)

	asduPayload := berTLV(0x80, []byte("MU0001"))                                  // svID
	asduPayload = append(asduPayload, berTLV(0x81, []byte("MU0001/LLN0$MU01"))...) // datSet
	asduPayload = append(asduPayload, berTLV(0x82, u16be(123))...)                 // smpCnt
	asduPayload = append(asduPayload, berTLV(0x83, u32be(5))...)                   // confRev
	// refrTm: 8 bytes UtcTime (epoch=1710000000, frac=0, quality=0x80=LeapSecondsKnown)
	refrTmBytes := make([]byte, 8)
	binary.BigEndian.PutUint32(refrTmBytes[0:4], 1710000000)
	refrTmBytes[7] = 0x80                                           // LeapSecondsKnown
	asduPayload = append(asduPayload, berTLV(0x84, refrTmBytes)...) // refrTm
	asduPayload = append(asduPayload, berTLV(0x85, []byte{2})...)   // smpSynch=global
	asduPayload = append(asduPayload, berTLV(0x86, u16be(80))...)   // smpRate=80
	asduPayload = append(asduPayload, berTLV(0x87, seqData)...)     // seqData
	asduPayload = append(asduPayload, berTLV(0x88, u16be(1))...)    // smpMod=FPC

	asdu := berTLV(0x30, asduPayload)
	seqASDU := berTLV(0xA2, asdu)
	noASDU := berTLV(0x80, []byte{1})
	savPDU := berTLV(0x60, append(noASDU, seqASDU...))

	apduLen := 8 + len(savPDU)
	apdu := make([]byte, 8, 8+len(savPDU))
	binary.BigEndian.PutUint16(apdu[0:], 0x4001)
	binary.BigEndian.PutUint16(apdu[2:], uint16(apduLen))
	apdu = append(apdu, savPDU...)

	frame := make([]byte, 14+len(apdu))
	copy(frame[0:6], []byte{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01})
	copy(frame[6:12], []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55})
	binary.BigEndian.PutUint16(frame[12:], EtherType)
	copy(frame[14:], apdu)

	asdus, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	a := asdus[0]
	if a.DatSet != "MU0001/LLN0$MU01" {
		t.Errorf("DatSet = %q, want MU0001/LLN0$MU01", a.DatSet)
	}
	if a.RefrTm.IsZero() {
		t.Error("RefrTm should not be zero")
	}
	if !a.RefrTmQ.LeapSecondsKnown {
		t.Error("RefrTmQ.LeapSecondsKnown should be true")
	}
	if a.RefrTmQ.ClockFailure || a.RefrTmQ.ClockNotSynchronized {
		t.Error("RefrTmQ should be valid")
	}
	if a.SmpRate != 80 {
		t.Errorf("SmpRate = %d, want 80", a.SmpRate)
	}
	if a.SmpMod != SmpModSamplesPerSecond {
		t.Errorf("SmpMod = %v, want fps(1)", a.SmpMod)
	}
}

func TestDecodeRejectsInvalidFixedWidthFields(t *testing.T) {
	tests := []struct {
		name  string
		field []byte
	}{
		{name: "short confRev", field: berTLV(0x83, []byte{1})},
		{name: "short smpMod", field: berTLV(0x88, []byte{1})},
		{name: "unknown smpMod", field: berTLV(0x88, u16be(3))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := decodeASDU(tt.field, 0, len(tt.field)); err == nil {
				t.Fatal("malformed field was accepted")
			}
		})
	}
}

func TestDecodeRejectsUnsafeLiveASDUAndKeepsOfflineWarning(t *testing.T) {
	validSeqData := make([]byte, SeqDataLen)
	base := func() []byte {
		payload := berTLV(0x80, []byte("MU0001"))
		payload = append(payload, berTLV(0x82, u16be(1))...)
		payload = append(payload, berTLV(0x83, u32be(1))...)
		payload = append(payload, berTLV(0x85, []byte{2})...)
		return payload
	}
	tests := []struct {
		name    string
		payload func() []byte
	}{
		{
			name: "missing seqData",
			payload: func() []byte {
				return base()
			},
		},
		{
			name: "short seqData",
			payload: func() []byte {
				return append(base(), berTLV(0x87, validSeqData[:8])...)
			},
		},
		{
			name: "invalid smpSynch",
			payload: func() []byte {
				payload := berTLV(0x80, []byte("MU0001"))
				payload = append(payload, berTLV(0x82, u16be(1))...)
				payload = append(payload, berTLV(0x83, u32be(1))...)
				payload = append(payload, berTLV(0x85, []byte{3})...)
				return append(payload, berTLV(0x87, validSeqData)...)
			},
		},
		{
			name: "short smpCnt",
			payload: func() []byte {
				payload := berTLV(0x80, []byte("MU0001"))
				payload = append(payload, berTLV(0x82, []byte{1})...)
				payload = append(payload, berTLV(0x83, u32be(1))...)
				payload = append(payload, berTLV(0x85, []byte{2})...)
				return append(payload, berTLV(0x87, validSeqData)...)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := buildTestFrameFromASDUPayload(0x4001, tt.payload())
			decoded, err := DecodeFrame(frame)
			if err != nil {
				t.Fatalf("offline DecodeFrame rejected diagnosable frame: %v", err)
			}
			if len(decoded.ASDUs) != 1 {
				t.Fatalf("offline ASDU count: want 1, got %d", len(decoded.ASDUs))
			}
			found := false
			for _, warning := range decoded.Warnings {
				if warning.RejectLive && warning.ASDUScoped && warning.ASDUIndex == 0 {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("missing ASDU-scoped live rejection warning: %+v", decoded.Warnings)
			}
			if _, err := Decode(frame); !errors.Is(err, ErrInvalidLive) {
				t.Fatalf("live Decode error: want %v, got %v", ErrInvalidLive, err)
			}
		})
	}
}

func TestDecodeNotSV(t *testing.T) {
	frame := make([]byte, 64)
	binary.BigEndian.PutUint16(frame[12:], 0x88B8) // GOOSE
	_, err := Decode(frame)
	if err == nil {
		t.Fatal("expected ErrNotSV")
	}
}

func TestDecodeShortFrame(t *testing.T) {
	_, err := Decode([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for short frame")
	}
}

func TestSubscriptionMatches(t *testing.T) {
	var ch [8]int32
	var q [8]Quality
	frame := buildTestFrame(0x4001, "MU0001", 0, 1, ch, q)
	asdus, _ := Decode(frame)
	a := &asdus[0]

	sub := &Subscription{
		Name:   "test",
		DstMAC: a.DstMAC,
		AppID:  0x4001,
		SvID:   "MU0001",
	}
	if !sub.Matches(a) {
		t.Error("expected match")
	}

	sub2 := &Subscription{
		Name:   "other",
		DstMAC: a.DstMAC,
		SvID:   "OTHER",
	}
	if sub2.Matches(a) {
		t.Error("expected no match for different SvID")
	}
}

func TestRanges(t *testing.T) {
	if !IsValidAppID(0x4000) || !IsValidAppID(0x7FFF) {
		t.Error("boundary SV AppIDs must be valid")
	}
	if IsValidAppID(0x3FFF) || IsValidAppID(0x8000) {
		t.Error("out-of-range AppIDs must be invalid")
	}
}
