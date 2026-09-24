package goose

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
)

func strictTestPDU() *PDU {
	return &PDU{
		DstMAC:              net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:              net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:               0x0001,
		GocbRef:             "IED/LLN0$GO$Strict",
		TimeAllowedToLiveMs: 1000,
		DatSet:              "IED/LLN0$DS",
		GoID:                "STRICT",
		Timestamp:           time.Unix(1_700_000_000, 0).UTC(),
		StNum:               1,
		SqNum:               0,
		ConfRev:             1,
	}
}

func TestDecodeRejectsTruncatedAPDU(t *testing.T) {
	frame := Encode(strictTestPDU())
	if _, err := Decode(frame[:len(frame)-1]); !errors.Is(err, ErrBadPDU) {
		t.Fatalf("truncated APDU: want ErrBadPDU, got %v", err)
	}
}

func TestDecodeRejectsMissingMandatoryFields(t *testing.T) {
	pdu := strictTestPDU()
	frame := make([]byte, 24)
	copy(frame[0:6], pdu.DstMAC)
	copy(frame[6:12], pdu.SrcMAC)
	binary.BigEndian.PutUint16(frame[12:14], EtherType)
	binary.BigEndian.PutUint16(frame[14:16], pdu.AppID)
	binary.BigEndian.PutUint16(frame[16:18], 10)
	frame[22] = 0x61
	frame[23] = 0

	if _, err := Decode(frame); !errors.Is(err, ErrBadPDU) {
		t.Fatalf("empty goosePDU: want ErrBadPDU, got %v", err)
	}
}

func TestDecodeRejectsDatasetEntryCountMismatch(t *testing.T) {
	pdu := strictTestPDU()
	pdu.NumDatSetEntries = 1
	if _, err := Decode(Encode(pdu)); !errors.Is(err, ErrBadPDU) {
		t.Fatalf("dataset entry mismatch: want ErrBadPDU, got %v", err)
	}
}

// TestEncodeDecodeRoundTrip verifies that Encode followed by Decode reproduces the PDU.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	ts := time.Date(2024, 6, 15, 12, 30, 45, 123456789, time.UTC)

	pdu := &PDU{
		DstMAC:              net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:              net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:               0x0001,
		GocbRef:             "IED1/LLN0$GO$Control",
		TimeAllowedToLiveMs: 1000,
		DatSet:              "IED1/LLN0$DataSet1",
		GoID:                "CTRL1",
		Timestamp:           ts,
		TimeQuality:         TimeQuality{LeapSecondsKnown: true, Accuracy: 10},
		StNum:               5,
		SqNum:               42,
		Test:                false,
		ConfRev:             1,
		NdsCom:              false,
		NumDatSetEntries:    2,
		AllData: []DataValue{
			{Type: DataTypeBoolean, Bool: true},
			{Type: DataTypeInteger, Int: -100},
		},
	}

	frame := Encode(pdu)
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	assertMAC(t, "DstMAC", pdu.DstMAC, got.DstMAC)
	assertMAC(t, "SrcMAC", pdu.SrcMAC, got.SrcMAC)
	assertEqual(t, "AppID", pdu.AppID, got.AppID)
	assertEqual(t, "GocbRef", pdu.GocbRef, got.GocbRef)
	assertEqual(t, "TimeAllowedToLiveMs", pdu.TimeAllowedToLiveMs, got.TimeAllowedToLiveMs)
	assertEqual(t, "DatSet", pdu.DatSet, got.DatSet)
	assertEqual(t, "GoID", pdu.GoID, got.GoID)
	assertEqual(t, "StNum", pdu.StNum, got.StNum)
	assertEqual(t, "SqNum", pdu.SqNum, got.SqNum)
	assertEqual(t, "Test", pdu.Test, got.Test)
	assertEqual(t, "ConfRev", pdu.ConfRev, got.ConfRev)
	assertEqual(t, "NdsCom", pdu.NdsCom, got.NdsCom)
	assertEqual(t, "NumDatSetEntries", pdu.NumDatSetEntries, got.NumDatSetEntries)
	assertEqual(t, "Simulation", pdu.Simulation, got.Simulation)
	assertEqual(t, "TimeQuality.LeapSecondsKnown", true, got.TimeQuality.LeapSecondsKnown)
	assertEqual(t, "TimeQuality.Accuracy", uint8(10), got.TimeQuality.Accuracy)

	if len(got.AllData) != 2 {
		t.Fatalf("AllData len: want 2, got %d", len(got.AllData))
	}
	assertEqual(t, "AllData[0].Type", DataTypeBoolean, got.AllData[0].Type)
	assertEqual(t, "AllData[0].Bool", true, got.AllData[0].Bool)
	assertEqual(t, "AllData[1].Type", DataTypeInteger, got.AllData[1].Type)
	assertEqual(t, "AllData[1].Int", int64(-100), got.AllData[1].Int)
}

// TestEncodeDecodeWithVLAN verifies a round trip for a VLAN-tagged frame.
func TestEncodeDecodeWithVLAN(t *testing.T) {
	pdu := &PDU{
		DstMAC: net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x02},
		SrcMAC: net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		VLAN: &ethernet.VLANTag{
			Priority: 4,
			DEI:      false,
			VID:      100,
		},
		AppID:               0x0010,
		GocbRef:             "IED2/LLN0$GO$Status",
		TimeAllowedToLiveMs: 500,
		DatSet:              "IED2/LLN0$DS2",
		GoID:                "ST2",
		Timestamp:           time.Now().UTC(),
		TimeQuality:         TimeQuality{LeapSecondsKnown: true, Accuracy: 10},
		StNum:               1,
		SqNum:               0,
		ConfRev:             3,
		NumDatSetEntries:    1,
		AllData: []DataValue{
			{Type: DataTypeUnsigned, UInt: 1<<63 + 12345},
		},
	}

	frame := Encode(pdu)
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if got.VLAN == nil {
		t.Fatal("VLAN is nil, expected non-nil")
	}
	assertEqual(t, "VLAN.Priority", uint8(4), got.VLAN.Priority)
	assertEqual(t, "VLAN.VID", uint16(100), got.VLAN.VID)
	assertEqual(t, "AppID", uint16(0x0010), got.AppID)
	assertEqual(t, "AllData[0].UInt", uint64(1<<63+12345), got.AllData[0].UInt)
}

// TestEncodeDecodeSimulation verifies the Simulation bit in Reserved1.
func TestEncodeDecodeSimulation(t *testing.T) {
	pdu := &PDU{
		DstMAC:              net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:              net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:               0x0001,
		Simulation:          true,
		GocbRef:             "IED/LLN0$GO$Sim",
		TimeAllowedToLiveMs: 1000,
		DatSet:              "IED/LLN0$DS",
		GoID:                "SIM",
		Timestamp:           time.Now().UTC(),
		TimeQuality:         TimeQuality{LeapSecondsKnown: true, Accuracy: 10},
		StNum:               0,
		SqNum:               0,
		ConfRev:             1,
		NumDatSetEntries:    0,
	}

	frame := Encode(pdu)
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	assertEqual(t, "Simulation", true, got.Simulation)
}

// TestEncodeDecodeAllDataTypes verifies a round trip for every supported data type.
func TestEncodeDecodeAllDataTypes(t *testing.T) {
	pdu := &PDU{
		DstMAC:              net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:              net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:               0x0001,
		GocbRef:             "IED/LLN0$GO$Test",
		TimeAllowedToLiveMs: 1000,
		DatSet:              "IED/LLN0$DS",
		GoID:                "TEST",
		Timestamp:           time.Now().UTC(),
		TimeQuality:         TimeQuality{LeapSecondsKnown: true, Accuracy: 10},
		StNum:               1,
		SqNum:               0,
		ConfRev:             1,
		NumDatSetEntries:    7,
		AllData: []DataValue{
			{Type: DataTypeBoolean, Bool: false},
			{Type: DataTypeInteger, Int: 42},
			{Type: DataTypeUnsigned, UInt: 65535},
			{Type: DataTypeFloatingPoint, Float: 3.14},
			{Type: DataTypeVisibleString, String: "hello"},
			{Type: DataTypeBitString, Bytes: []byte{0xAB, 0xCD}},
			{Type: DataTypeStructure, Children: []DataValue{
				{Type: DataTypeBoolean, Bool: true},
				{Type: DataTypeInteger, Int: -1},
			}},
		},
	}

	frame := Encode(pdu)
	got, err := Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(got.AllData) != 7 {
		t.Fatalf("AllData len: want 7, got %d", len(got.AllData))
	}

	assertEqual(t, "bool", false, got.AllData[0].Bool)
	assertEqual(t, "int", int64(42), got.AllData[1].Int)
	assertEqual(t, "uint", uint64(65535), got.AllData[2].UInt)

	// float32 precision: 3.14 → float32 → float64
	if got.AllData[3].Float < 3.13 || got.AllData[3].Float > 3.15 {
		t.Errorf("float: want ~3.14, got %f", got.AllData[3].Float)
	}

	assertEqual(t, "string", "hello", got.AllData[4].String)

	if len(got.AllData[5].Bytes) != 2 || got.AllData[5].Bytes[0] != 0xAB {
		t.Errorf("bit_string: want [AB CD], got %X", got.AllData[5].Bytes)
	}
	assertEqual(t, "bit_string length", 16, got.AllData[5].BitLength)

	// structure children
	if len(got.AllData[6].Children) != 2 {
		t.Fatalf("structure children: want 2, got %d", len(got.AllData[6].Children))
	}
	assertEqual(t, "struct[0].Bool", true, got.AllData[6].Children[0].Bool)
	assertEqual(t, "struct[1].Int", int64(-1), got.AllData[6].Children[1].Int)
}

// TestBerEncodeUint verifies minimal BER encoding of unsigned integers.
func TestBerEncodeUint(t *testing.T) {
	tests := []struct {
		in   uint32
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7F}},
		{128, []byte{0x00, 0x80}},
		{255, []byte{0x00, 0xFF}},
		{32768, []byte{0x00, 0x80, 0x00}},
		{0xFFFFFFFF, []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, tt := range tests {
		b := berEncodeUint(tt.in)
		if !bytes.Equal(b, tt.want) {
			t.Errorf("berEncodeUint(%d): want %X, got %X", tt.in, tt.want, b)
		}
		got, err := berUint32(b)
		if err != nil || got != tt.in {
			t.Errorf("berUint32(berEncodeUint(%d)): got %d, err=%v", tt.in, got, err)
		}
	}
}

func TestUnsignedAllDataSupportsFullUint64(t *testing.T) {
	encoded := encodeDataValue(DataValue{Type: DataTypeUnsigned, UInt: ^uint64(0)})
	want := append([]byte{0x86, 0x09, 0x00}, bytes.Repeat([]byte{0xFF}, 8)...)
	if !bytes.Equal(encoded, want) {
		t.Fatalf("uint64 BER: got %X, want %X", encoded, want)
	}
	decoded, err := decodeDataValue(encoded[0], encoded[2:])
	if err != nil || decoded.UInt != ^uint64(0) {
		t.Fatalf("uint64 roundtrip: got %+v, err=%v", decoded, err)
	}
}

func TestQualityBitStringGoldenEncoding(t *testing.T) {
	// invalid + test + operatorBlocked + derived, with IEC bit numbers 1,11,12,13.
	value := NewQualityBitString(2 | 1<<11 | 1<<12 | 1<<13)
	encoded := encodeDataValue(value)
	want := []byte{0x84, 0x03, 0x02, 0x40, 0x1C}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("quality BIT STRING: got %X, want %X", encoded, want)
	}
	decoded, err := decodeDataValue(encoded[0], encoded[2:])
	if err != nil {
		t.Fatal(err)
	}
	if decoded.BitLength != QualityBitLength || !bytes.Equal(decoded.Bytes, []byte{0x40, 0x1C}) {
		t.Fatalf("quality BIT STRING roundtrip: %+v", decoded)
	}
}

func TestDecodeRejectsMalformedBitStringsAndUnsigned(t *testing.T) {
	for _, value := range [][]byte{nil, {8, 0}, {1, 1}} {
		if _, err := decodeDataValue(0x84, value); err == nil {
			t.Fatalf("malformed BIT STRING %X was accepted", value)
		}
	}
	for _, value := range [][]byte{{0x80}, {0, 1, 2, 3, 4, 5}, {0, 0x7F}} {
		if _, err := berUint32(value); err == nil {
			t.Fatalf("malformed unsigned %X was accepted", value)
		}
	}
}

// TestBerEncodeInt verifies minimal BER encoding of signed integers.
func TestBerEncodeInt(t *testing.T) {
	tests := []struct {
		in int64
	}{
		{0},
		{1},
		{-1},
		{127},
		{128},
		{-128},
		{-129},
		{32767},
		{-32768},
		{100000},
		{-100000},
	}
	for _, tt := range tests {
		b := berEncodeInt(tt.in)
		got := berInt(b)
		if got != tt.in {
			t.Errorf("berInt(berEncodeInt(%d)): got %d (encoded: %X)", tt.in, got, b)
		}
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
