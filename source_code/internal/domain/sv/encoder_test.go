package sv

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
)

// TestEncodeDecodeRoundTrip verifies that Encode followed by Decode reproduces the ASDU.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	asdu := ASDU{
		DstMAC:   net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:    0x4001,
		SvID:     "MU0001",
		SmpCnt:   42,
		ConfRev:  1,
		SmpSynch: SmpSynchGlobal,
		SmpRate:  80,
		Channels: [NumChannels]int32{1000, -2000, 3000, -4000, 5000, -6000, 7000, -8000},
		Quality:  [NumChannels]Quality{QualityValid, QualityValid, QualityValid, QualityValid, QualityValid, QualityValid, QualityValid, QualityValid},
	}

	frame := Encode([]ASDU{asdu})
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ASDU count: want 1, got %d", len(got))
	}

	g := got[0]
	assertEqual(t, "AppID", asdu.AppID, g.AppID)
	assertEqual(t, "SvID", asdu.SvID, g.SvID)
	assertEqual(t, "SmpCnt", asdu.SmpCnt, g.SmpCnt)
	assertEqual(t, "ConfRev", asdu.ConfRev, g.ConfRev)
	assertEqual(t, "SmpSynch", asdu.SmpSynch, g.SmpSynch)
	assertEqual(t, "SmpRate", asdu.SmpRate, g.SmpRate)

	for ch := 0; ch < NumChannels; ch++ {
		assertEqual(t, "Channels", asdu.Channels[ch], g.Channels[ch])
		assertEqual(t, "Quality", asdu.Quality[ch], g.Quality[ch])
	}

	assertMAC(t, "DstMAC", asdu.DstMAC, g.DstMAC)
	assertMAC(t, "SrcMAC", asdu.SrcMAC, g.SrcMAC)
}

// TestEncodeDecodeWithVLAN verifies a round trip with a VLAN tag.
func TestEncodeDecodeWithVLAN(t *testing.T) {
	asdu := ASDU{
		DstMAC: net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x02},
		SrcMAC: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		VLAN: &ethernet.VLANTag{
			Priority: 4,
			VID:      200,
		},
		AppID:    0x4002,
		SvID:     "MU0002",
		SmpCnt:   0,
		ConfRev:  5,
		SmpSynch: SmpSynchLocal,
		SmpRate:  256,
		Channels: [NumChannels]int32{100, 200, 300, 400, 500, 600, 700, 800},
	}

	frame := Encode([]ASDU{asdu})
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ASDU count: want 1, got %d", len(got))
	}

	g := got[0]
	if g.VLAN == nil {
		t.Fatal("VLAN is nil, expected non-nil")
	}
	assertEqual(t, "VLAN.Priority", uint8(4), g.VLAN.Priority)
	assertEqual(t, "VLAN.VID", uint16(200), g.VLAN.VID)
	assertEqual(t, "SmpRate", uint16(256), g.SmpRate)
}

// TestEncodeDecodeSimulation verifies the Simulation bit.
func TestEncodeDecodeSimulation(t *testing.T) {
	asdu := ASDU{
		DstMAC:     net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:     net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:      0x4001,
		Simulation: true,
		SvID:       "SIM01",
		SmpCnt:     10,
		ConfRev:    1,
		SmpSynch:   SmpSynchNone,
		SmpRate:    80,
	}

	frame := Encode([]ASDU{asdu})
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	assertEqual(t, "Simulation", true, got[0].Simulation)
}

// TestEncodeDecodeMultiASDU verifies a round trip of multiple ASDUs in one frame.
func TestEncodeDecodeMultiASDU(t *testing.T) {
	asdus := []ASDU{
		{
			DstMAC:   net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
			SrcMAC:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			AppID:    0x4001,
			SvID:     "MU01",
			SmpCnt:   0,
			ConfRev:  1,
			SmpSynch: SmpSynchNone,
			SmpRate:  80,
			Channels: [NumChannels]int32{111, 222, 333, 444, 555, 666, 777, 888},
		},
		{
			DstMAC:   net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
			SrcMAC:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			AppID:    0x4001,
			SvID:     "MU01",
			SmpCnt:   1,
			ConfRev:  1,
			SmpSynch: SmpSynchNone,
			SmpRate:  80,
			Channels: [NumChannels]int32{-111, -222, -333, -444, -555, -666, -777, -888},
		},
	}

	frame := Encode(asdus)
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ASDU count: want 2, got %d", len(got))
	}

	assertEqual(t, "ASDU[0].SmpCnt", uint16(0), got[0].SmpCnt)
	assertEqual(t, "ASDU[1].SmpCnt", uint16(1), got[1].SmpCnt)
	assertEqual(t, "ASDU[0].Channels[0]", int32(111), got[0].Channels[0])
	assertEqual(t, "ASDU[1].Channels[0]", int32(-111), got[1].Channels[0])
}

// TestEncodeDecodeQuality verifies a round trip of the quality bits.
func TestEncodeDecodeQuality(t *testing.T) {
	asdu := ASDU{
		DstMAC:   net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:    0x4001,
		SvID:     "MU01",
		SmpCnt:   5,
		ConfRev:  1,
		SmpSynch: SmpSynchNone,
		SmpRate:  80,
		Quality:  [NumChannels]Quality{QualityInvalid, QualityTest, QualityOverflow, QualityValid, QualityValid, QualityValid, QualityValid, QualityValid},
	}

	frame := Encode([]ASDU{asdu})
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	assertEqual(t, "Quality[0]", QualityInvalid, got[0].Quality[0])
	assertEqual(t, "Quality[1]", QualityTest, got[0].Quality[1])
	assertEqual(t, "Quality[2]", QualityOverflow, got[0].Quality[2])
	assertEqual(t, "Quality[3]", QualityValid, got[0].Quality[3])
}

func TestEncodeDecodeOptionalFields(t *testing.T) {
	referenceTime := time.Date(2026, 8, 5, 10, 30, 0, 123_456_000, time.UTC)
	asdu := benchmarkASDU()
	asdu.DatSet = "MU0001/LLN0$MU01"
	asdu.ConfRev = 0xFEDCBA98
	asdu.RefrTm = referenceTime
	asdu.RefrTmQ = TimeQuality{LeapSecondsKnown: true, Accuracy: 10}
	asdu.SmpMod = SmpModSamplesPerSecond

	got, err := Decode(Encode([]ASDU{asdu}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ASDU count: want 1, got %d", len(got))
	}
	decoded := got[0]
	assertEqual(t, "DatSet", asdu.DatSet, decoded.DatSet)
	assertEqual(t, "ConfRev", asdu.ConfRev, decoded.ConfRev)
	assertEqual(t, "SmpMod", asdu.SmpMod, decoded.SmpMod)
	if delta := decoded.RefrTm.Sub(referenceTime); delta < -time.Microsecond || delta > time.Microsecond {
		t.Fatalf("RefrTm: want %v, got %v", referenceTime, decoded.RefrTm)
	}
}

func TestEncodeLengthFieldsMatchSerializedFrame(t *testing.T) {
	withOptionalFields := benchmarkASDU()
	withOptionalFields.VLAN = &ethernet.VLANTag{Priority: 4, VID: 1}
	withOptionalFields.DatSet = "PhsMeas1"
	withOptionalFields.RefrTm = time.Date(2026, 8, 6, 15, 0, 0, 0, time.UTC)

	for _, asdu := range []ASDU{benchmarkASDU(), withOptionalFields} {
		frame := Encode([]ASDU{asdu})
		headerLen := 14
		if asdu.VLAN != nil {
			headerLen = 18
		}
		declaredAPDULen := int(binary.BigEndian.Uint16(frame[headerLen+2 : headerLen+4]))
		if want := len(frame) - headerLen; declaredAPDULen != want {
			t.Fatalf("APDU Length: declared %d, serialized %d", declaredAPDULen, want)
		}

		savOffset := headerLen + 8
		if frame[savOffset] != 0x60 {
			t.Fatalf("savPDU tag: want 0x60, got 0x%02X", frame[savOffset])
		}
		savLen, valueOffset, err := berLength(frame, savOffset+1)
		if err != nil {
			t.Fatal(err)
		}
		if wantEnd := valueOffset + savLen; wantEnd != len(frame) {
			t.Fatalf("savPDU end: declared %d, serialized %d", wantEnd, len(frame))
		}
	}
}

func TestEncodeMatchesReferenceFrame(t *testing.T) {
	asdu := ASDU{
		DstMAC:  net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:  net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01},
		AppID:   0x4000,
		SvID:    "PBMTMU0001",
		SmpCnt:  0,
		ConfRev: 1,
	}

	// Synthetic reference with a locally administered source MAC and neutral svID.
	// Its 64-byte seqData is all zero; the ASDU intentionally contains no datSet,
	// refrTm, smpRate, or smpMod. Keep the expected bytes independent of Encode.
	referenceHex := "010ccd04000102000000000188ba" +
		"4000006c000000006062800101a25d305b" +
		"800a50424d544d5530303031820200008304000000018501008740" +
		strings.Repeat("00", 64)
	want, err := hex.DecodeString(referenceHex)
	if err != nil {
		t.Fatal(err)
	}
	got := Encode([]ASDU{asdu})
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded frame differs from synthetic reference frame:\nwant %X\n got %X", want, got)
	}
}

func TestEncodeUsesFixedWidthConfRevAndSmpMod(t *testing.T) {
	asdu := benchmarkASDU()
	asdu.ConfRev = 0xFEDCBA98
	asdu.SmpMod = SmpModSamplesPerSecond

	frame := Encode([]ASDU{asdu})
	if !bytes.Contains(frame, []byte{0x83, 0x04, 0xFE, 0xDC, 0xBA, 0x98}) {
		t.Fatalf("confRev is not encoded as a four-octet INT32U: %X", frame)
	}
	if !bytes.Contains(frame, []byte{0x88, 0x02, 0x00, 0x01}) {
		t.Fatalf("smpMod is not encoded as a two-octet INT16U: %X", frame)
	}
}

func TestEncodeSingleASDUAllocationBudget(t *testing.T) {
	asdus := []ASDU{benchmarkASDU()}
	var frame []byte
	allocations := testing.AllocsPerRun(100, func() {
		frame = Encode(asdus)
	})
	if len(frame) == 0 {
		t.Fatal("Encode returned an empty frame")
	}
	if allocations > 1 {
		t.Fatalf("Encode allocated %.2f objects; want at most 1", allocations)
	}
}

var benchmarkFrame []byte

func BenchmarkEncodeSingleASDU(b *testing.B) {
	asdus := []ASDU{benchmarkASDU()}
	b.ReportAllocs()
	b.SetBytes(int64(len(Encode(asdus))))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		asdus[0].SmpCnt++
		benchmarkFrame = Encode(asdus)
	}
}

func BenchmarkDecodeSingleASDU(b *testing.B) {
	frame := Encode([]ASDU{benchmarkASDU()})
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decode(frame)
	}
}

func benchmarkASDU() ASDU {
	return ASDU{
		DstMAC:   net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:    0x4001,
		SvID:     "MU0001",
		ConfRev:  1,
		SmpSynch: SmpSynchGlobal,
		SmpRate:  80,
		Channels: [NumChannels]int32{1000, -2000, 3000, -4000, 5000, -6000, 7000, -8000},
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func assertEqual[T comparable](t *testing.T, name string, want, got T) {
	t.Helper()
	if want != got {
		t.Errorf("%s: want %v, got %v", name, want, got)
	}
}

func assertMAC(t *testing.T, name string, want, got net.HardwareAddr) {
	t.Helper()
	if want.String() != got.String() {
		t.Errorf("%s: want %s, got %s", name, want, got)
	}
}
