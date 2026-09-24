package ptp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTimestampRoundTrip(t *testing.T) {
	now := time.Date(2025, 3, 15, 12, 30, 45, 123456789, time.UTC)
	ts := TimestampFromTime(now)
	got := ts.ToTime()
	if !got.Equal(now) {
		t.Fatalf("roundtrip failed: want %v, got %v", now, got)
	}
}

func TestTimestampTAIRoundTrip(t *testing.T) {
	now := time.Date(2025, 3, 15, 12, 30, 45, 123456789, time.UTC)
	ts := TimestampFromTimeWithUTCOffset(now, 37)
	if ts.Seconds != uint64(now.Unix()+37) {
		t.Fatalf("TAI seconds: want %d, got %d", now.Unix()+37, ts.Seconds)
	}
	got := ts.ToTimeWithUTCOffset(37)
	if !got.Equal(now) {
		t.Fatalf("TAI roundtrip failed: want %v, got %v", now, got)
	}
}

func TestClientClockIdentityIsDistinctAndParseable(t *testing.T) {
	mac := [6]byte{0x68, 0x2F, 0x67, 0x92, 0x78, 0x3F}
	server := ClockIdentityFromMAC(mac)
	client := ClientClockIdentityFromMAC(mac)
	if client == server {
		t.Fatal("automatic Client and Server ClockIdentity must differ")
	}
	parsed, err := parseClockIdentity(client.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != client {
		t.Fatalf("identity round trip: want %s, got %s", client, parsed)
	}
}

func parseClockIdentity(value string) (ClockIdentity, error) {
	normalized := strings.NewReplacer("-", "", ":", "", ".", "").Replace(strings.TrimSpace(value))
	if len(normalized) != 16 {
		return ClockIdentity{}, fmt.Errorf("clock identity must contain 8 bytes, got %q", value)
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil {
		return ClockIdentity{}, fmt.Errorf("invalid clock identity %q: %w", value, err)
	}
	var identity ClockIdentity
	copy(identity[:], decoded)
	return identity, nil
}

func TestClockIdentityFromMAC(t *testing.T) {
	mac := [6]byte{0xAA, 0xBB, 0xCC, 0x11, 0x22, 0x33}
	ci := ClockIdentityFromMAC(mac)
	want := ClockIdentity{0xAA, 0xBB, 0xCC, 0xFF, 0xFE, 0x11, 0x22, 0x33}
	if ci != want {
		t.Fatalf("ClockIdentityFromMAC: want %v, got %v", want, ci)
	}
}

func TestHeaderCodec(t *testing.T) {
	hdr := Header{
		MessageType:       MsgSync,
		TransportSpecific: 1,
		VersionPTP:        PTPVersion2,
		MessageLength:     44,
		DomainNumber:      254,
		FlagField:         FlagTwoStep,
		CorrectionField:   12345 << 16,
		SourcePortIdentity: PortIdentity{
			ClockIdentity: ClockIdentity{1, 2, 3, 4, 5, 6, 7, 8},
			PortNumber:    1,
		},
		SequenceID:         100,
		ControlField:       ControlSync,
		LogMessageInterval: -4,
	}

	buf := make([]byte, hdr.MessageLength)
	EncodeHeader(hdr, buf)

	got, err := DecodeHeader(buf)
	if err != nil {
		t.Fatal(err)
	}

	if got.MessageType != hdr.MessageType {
		t.Errorf("MessageType: want %d, got %d", hdr.MessageType, got.MessageType)
	}
	if got.TransportSpecific != hdr.TransportSpecific {
		t.Errorf("TransportSpecific: want %d, got %d", hdr.TransportSpecific, got.TransportSpecific)
	}
	if got.VersionPTP != hdr.VersionPTP {
		t.Errorf("VersionPTP: want %d, got %d", hdr.VersionPTP, got.VersionPTP)
	}
	if got.MessageLength != hdr.MessageLength {
		t.Errorf("MessageLength: want %d, got %d", hdr.MessageLength, got.MessageLength)
	}
	if got.DomainNumber != hdr.DomainNumber {
		t.Errorf("DomainNumber: want %d, got %d", hdr.DomainNumber, got.DomainNumber)
	}
	if got.FlagField != hdr.FlagField {
		t.Errorf("FlagField: want 0x%04X, got 0x%04X", hdr.FlagField, got.FlagField)
	}
	if got.CorrectionField != hdr.CorrectionField {
		t.Errorf("CorrectionField: want %d, got %d", hdr.CorrectionField, got.CorrectionField)
	}
	if got.SourcePortIdentity != hdr.SourcePortIdentity {
		t.Errorf("SourcePortIdentity: want %v, got %v", hdr.SourcePortIdentity, got.SourcePortIdentity)
	}
	if got.SequenceID != hdr.SequenceID {
		t.Errorf("SequenceID: want %d, got %d", hdr.SequenceID, got.SequenceID)
	}
	if got.LogMessageInterval != hdr.LogMessageInterval {
		t.Errorf("LogMessageInterval: want %d, got %d", hdr.LogMessageInterval, got.LogMessageInterval)
	}
}

func TestDecodeHeaderRejectsTruncatedMessage(t *testing.T) {
	hdr := Header{MessageType: MsgDelayReq, VersionPTP: PTPVersion2}
	data := EncodeDelayReq(hdr, DelayReqBody{})
	if _, err := DecodeHeader(data[:HeaderSize]); err == nil {
		t.Fatal("expected truncated message error")
	}
}

func TestDecodeHeaderTooShort(t *testing.T) {
	_, err := DecodeHeader(make([]byte, 10))
	if err == nil {
		t.Fatal("expected error for short buffer")
	}
}

func TestTimestampCodec(t *testing.T) {
	ts := Timestamp{Seconds: 1710505845, Nanoseconds: 123456789}
	buf := make([]byte, 10)
	encodeTimestamp(ts, buf)
	got := DecodeTimestamp(buf)
	if got != ts {
		t.Fatalf("Timestamp roundtrip: want %v, got %v", ts, got)
	}
}

func TestDelayReqRoundTrip(t *testing.T) {
	hdr := Header{
		MessageType:        MsgDelayReq,
		VersionPTP:         PTPVersion2,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{1, 2, 3, 4, 5, 6, 7, 8}, PortNumber: 1},
		SequenceID:         42,
		ControlField:       ControlDelayReq,
	}
	body := DelayReqBody{OriginTimestamp: TimestampFromTime(time.Now().UTC())}
	data := EncodeDelayReq(hdr, body)

	gotHdr, err := DecodeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.MessageType != MsgDelayReq {
		t.Errorf("MessageType: want %d, got %d", MsgDelayReq, gotHdr.MessageType)
	}
	if gotHdr.SequenceID != 42 {
		t.Errorf("SequenceID: want 42, got %d", gotHdr.SequenceID)
	}
}

func TestDecodeAnnounceBody(t *testing.T) {
	body := AnnounceBody{
		CurrentUtcOffset:     37,
		GrandmasterPriority1: 128,
		GrandmasterClockQuality: ClockQuality{
			ClockClass:              ClockClass6,
			ClockAccuracy:           ClockAccuracy100ns,
			OffsetScaledLogVariance: 0x49A0,
		},
		GrandmasterPriority2: 128,
		GrandmasterIdentity:  ClockIdentity{0xAA, 0xBB, 0xCC, 0xFF, 0xFE, 0x11, 0x22, 0x33},
		StepsRemoved:         0,
		TimeSource:           TimeSourceGPS,
	}

	// Encode announce body manually
	buf := make([]byte, 30)
	encodeTimestamp(body.OriginTimestamp, buf[0:])
	buf[10] = byte(body.CurrentUtcOffset >> 8)
	buf[11] = byte(body.CurrentUtcOffset)
	buf[12] = body.Reserved
	buf[13] = body.GrandmasterPriority1
	buf[14] = body.GrandmasterClockQuality.ClockClass
	buf[15] = body.GrandmasterClockQuality.ClockAccuracy
	buf[16] = byte(body.GrandmasterClockQuality.OffsetScaledLogVariance >> 8)
	buf[17] = byte(body.GrandmasterClockQuality.OffsetScaledLogVariance)
	buf[18] = body.GrandmasterPriority2
	copy(buf[19:27], body.GrandmasterIdentity[:])
	buf[27] = byte(body.StepsRemoved >> 8)
	buf[28] = byte(body.StepsRemoved)
	buf[29] = body.TimeSource

	got, err := DecodeAnnounceBody(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.GrandmasterPriority1 != 128 {
		t.Errorf("Priority1: want 128, got %d", got.GrandmasterPriority1)
	}
	if got.GrandmasterClockQuality.ClockClass != ClockClass6 {
		t.Errorf("ClockClass: want %d, got %d", ClockClass6, got.GrandmasterClockQuality.ClockClass)
	}
	if got.GrandmasterIdentity != body.GrandmasterIdentity {
		t.Errorf("GmIdentity: want %v, got %v", body.GrandmasterIdentity, got.GrandmasterIdentity)
	}
	if got.TimeSource != TimeSourceGPS {
		t.Errorf("TimeSource: want 0x%02X, got 0x%02X", TimeSourceGPS, got.TimeSource)
	}
}

func TestDecodeDelayRespBody(t *testing.T) {
	ts := Timestamp{Seconds: 1000, Nanoseconds: 500}
	pi := PortIdentity{ClockIdentity: ClockIdentity{8, 7, 6, 5, 4, 3, 2, 1}, PortNumber: 3}

	buf := make([]byte, 20)
	encodeTimestamp(ts, buf[0:])
	copy(buf[10:18], pi.ClockIdentity[:])
	buf[18] = byte(pi.PortNumber >> 8)
	buf[19] = byte(pi.PortNumber)

	got, err := DecodeDelayRespBody(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiveTimestamp != ts {
		t.Errorf("ReceiveTimestamp: want %v, got %v", ts, got.ReceiveTimestamp)
	}
	if got.RequestingPortIdentity != pi {
		t.Errorf("RequestingPortIdentity: want %v, got %v", pi, got.RequestingPortIdentity)
	}
}

// --- Server encode roundtrip tests ---

func TestEncodeAnnounceRoundTrip(t *testing.T) {
	hdr := Header{
		MessageType:        MsgAnnounce,
		VersionPTP:         PTPVersion2,
		DomainNumber:       0,
		FlagField:          FlagPTPTimescale | FlagCurrentUtcOffsetValid | FlagTimeTraceable | FlagFrequencyTraceable,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{1, 2, 3, 0xFF, 0xFE, 4, 5, 6}, PortNumber: 1},
		SequenceID:         10,
		ControlField:       ControlOther,
		LogMessageInterval: 1,
	}
	body := AnnounceBody{
		OriginTimestamp:      TimestampFromTime(time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)),
		CurrentUtcOffset:     37,
		GrandmasterPriority1: 128,
		GrandmasterClockQuality: ClockQuality{
			ClockClass:              ClockClass6,
			ClockAccuracy:           ClockAccuracy100ns,
			OffsetScaledLogVariance: 0x49A0,
		},
		GrandmasterPriority2: 128,
		GrandmasterIdentity:  ClockIdentity{1, 2, 3, 0xFF, 0xFE, 4, 5, 6},
		StepsRemoved:         0,
		TimeSource:           TimeSourceGPS,
	}

	data := EncodeAnnounce(hdr, body)

	gotHdr, err := DecodeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.MessageType != MsgAnnounce {
		t.Errorf("MessageType: want %d, got %d", MsgAnnounce, gotHdr.MessageType)
	}
	if gotHdr.MessageLength != uint16(HeaderSize+announceBodySize) {
		t.Errorf("MessageLength: want %d, got %d", HeaderSize+announceBodySize, gotHdr.MessageLength)
	}

	gotBody, err := DecodeAnnounceBody(data[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.CurrentUtcOffset != 37 {
		t.Errorf("CurrentUtcOffset: want 37, got %d", gotBody.CurrentUtcOffset)
	}
	if gotBody.GrandmasterPriority1 != 128 {
		t.Errorf("Priority1: want 128, got %d", gotBody.GrandmasterPriority1)
	}
	if gotBody.GrandmasterClockQuality.ClockClass != ClockClass6 {
		t.Errorf("ClockClass: want %d, got %d", ClockClass6, gotBody.GrandmasterClockQuality.ClockClass)
	}
	if gotBody.GrandmasterIdentity != body.GrandmasterIdentity {
		t.Errorf("GmIdentity: want %v, got %v", body.GrandmasterIdentity, gotBody.GrandmasterIdentity)
	}
	if gotBody.TimeSource != TimeSourceGPS {
		t.Errorf("TimeSource: want 0x%02X, got 0x%02X", TimeSourceGPS, gotBody.TimeSource)
	}
}

func TestEncodeAnnounceC372382011MatchesWorkingIEDCapture(t *testing.T) {
	hdr := Header{
		MessageType:        MsgAnnounce,
		TransportSpecific:  0,
		VersionPTP:         PTPVersion2,
		DomainNumber:       0,
		FlagField:          FlagPTPTimescale | FlagCurrentUtcOffsetValid | FlagTimeTraceable | FlagFrequencyTraceable,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{0xAA, 0xBB, 0xCC, 0xFF, 0xFE, 0x11, 0x22, 0x33}, PortNumber: 1},
		SequenceID:         5,
		ControlField:       ControlOther,
		LogMessageInterval: -3,
	}
	body := AnnounceBody{
		CurrentUtcOffset:     37,
		GrandmasterPriority1: 128,
		GrandmasterClockQuality: ClockQuality{
			ClockClass:    ClockClass6,
			ClockAccuracy: ClockAccuracy25ns,
		},
		GrandmasterPriority2: 128,
		GrandmasterIdentity:  hdr.SourcePortIdentity.ClockIdentity,
		TimeSource:           TimeSourceGPS,
	}
	tlv := C37238TLV2011{
		GrandmasterID:             3,
		GrandmasterTimeInaccuracy: 60,
		NetworkTimeInaccuracy:     0,
	}
	alternate := AlternateTimeOffsetTLV{
		KeyField:       1,
		CurrentOffset:  10763,
		JumpSeconds:    0,
		TimeOfNextJump: 0,
		DisplayName:    "UTC+03:00",
	}

	data := EncodeAnnounceC372382011(hdr, body, tlv, &alternate)

	gotHdr, err := DecodeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.TransportSpecific != 0 {
		t.Errorf("TransportSpecific: want 0, got %d", gotHdr.TransportSpecific)
	}
	if gotHdr.DomainNumber != 0 {
		t.Errorf("DomainNumber: want 0, got %d", gotHdr.DomainNumber)
	}

	// C37 TLV: 4 + 18 bytes; ATOI with displayName UTC+03:00: 4 + 26 bytes.
	const wantLength = HeaderSize + announceBodySize + 22 + 30
	if len(data) != wantLength || int(gotHdr.MessageLength) != wantLength {
		t.Fatalf("Announce length: want %d, got buffer=%d header=%d", wantLength, len(data), gotHdr.MessageLength)
	}

	c37Offset := HeaderSize + announceBodySize
	if got := binary.BigEndian.Uint16(data[c37Offset:]); got != TLVOrganizationExtension {
		t.Fatalf("TLV type: want ORGANIZATION_EXTENSION (0x%04X), got 0x%04X", TLVOrganizationExtension, got)
	}
	if got := binary.BigEndian.Uint16(data[c37Offset+2:]); got != 18 {
		t.Fatalf("C37.238 TLV length: want 18, got %d", got)
	}
	if got := [3]byte(data[c37Offset+4 : c37Offset+7]); got != C37238OrgID {
		t.Fatalf("C37.238 OUI: want % X, got % X", C37238OrgID, got)
	}
	if got := [3]byte(data[c37Offset+7 : c37Offset+10]); got != C37238OrgSubType2011 {
		t.Fatalf("C37.238 subtype: want % X, got % X", C37238OrgSubType2011, got)
	}
	if got := binary.BigEndian.Uint16(data[c37Offset+10:]); got != tlv.GrandmasterID {
		t.Fatalf("Grandmaster ID: want 0x%04X, got 0x%04X", tlv.GrandmasterID, got)
	}
	if got := binary.BigEndian.Uint32(data[c37Offset+12:]); got != tlv.GrandmasterTimeInaccuracy {
		t.Fatalf("grandmaster time inaccuracy: want %d, got %d", tlv.GrandmasterTimeInaccuracy, got)
	}
	if got := binary.BigEndian.Uint32(data[c37Offset+16:]); got != tlv.NetworkTimeInaccuracy {
		t.Fatalf("network time inaccuracy: want %d, got %d", tlv.NetworkTimeInaccuracy, got)
	}

	alternateOffset := c37Offset + 22
	if got := binary.BigEndian.Uint16(data[alternateOffset:]); got != TLVAlternateTimeOffset {
		t.Fatalf("alternate TLV type: want 0x%04X, got 0x%04X", TLVAlternateTimeOffset, got)
	}
	if got := binary.BigEndian.Uint16(data[alternateOffset+2:]); got != 26 {
		t.Fatalf("alternate TLV length: want 26, got %d", got)
	}
	if got := data[alternateOffset+4]; got != 1 {
		t.Fatalf("alternate keyField: want 1, got %d", got)
	}
	if got := int32(binary.BigEndian.Uint32(data[alternateOffset+5:])); got != 10763 {
		t.Fatalf("alternate currentOffset: want 10763, got %d", got)
	}
	if got := data[alternateOffset+19]; got != 9 {
		t.Fatalf("alternate displayName length: want 9, got %d", got)
	}
	if got := string(data[alternateOffset+20 : alternateOffset+29]); got != "UTC+03:00" {
		t.Fatalf("alternate displayName: want UTC+03:00, got %q", got)
	}
}

func TestEncodeAnnounceC372382017(t *testing.T) {
	hdr := Header{
		MessageType:        MsgAnnounce,
		VersionPTP:         PTPVersion2,
		DomainNumber:       254,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{1, 2, 3, 4, 5, 6, 7, 8}, PortNumber: 1},
	}
	body := AnnounceBody{GrandmasterIdentity: hdr.SourcePortIdentity.ClockIdentity}
	tlv := C37238TLV2017{GrandmasterID: 3, TotalTimeInaccuracy: 100}
	data := EncodeAnnounceC372382017(hdr, body, tlv, nil)

	const c37Offset = HeaderSize + announceBodySize
	if len(data) != c37Offset+22 {
		t.Fatalf("Announce length: want %d, got %d", c37Offset+22, len(data))
	}
	if got := [3]byte(data[c37Offset+7 : c37Offset+10]); got != C37238OrgSubType2017 {
		t.Fatalf("C37.238 subtype: want % X, got % X", C37238OrgSubType2017, got)
	}
	if got := binary.BigEndian.Uint32(data[c37Offset+16:]); got != 100 {
		t.Fatalf("total time inaccuracy: want 100, got %d", got)
	}
}

func TestEncodeAnnounceC372382017AppendsAlternateTimeOffset(t *testing.T) {
	alternate := &AlternateTimeOffsetTLV{
		KeyField:      1,
		CurrentOffset: 10763,
		DisplayName:   "UTC+03:00",
	}
	data := EncodeAnnounceC372382017(
		Header{MessageType: MsgAnnounce, VersionPTP: PTPVersion2},
		AnnounceBody{},
		C37238TLV2017{GrandmasterID: 3, TotalTimeInaccuracy: 100},
		alternate,
	)
	alternateOffset := HeaderSize + announceBodySize + 22
	if len(data) != alternateOffset+30 {
		t.Fatalf("Announce length: want %d, got %d", alternateOffset+30, len(data))
	}
	if got := binary.BigEndian.Uint16(data[alternateOffset:]); got != TLVAlternateTimeOffset {
		t.Fatalf("next TLV: want ALTERNATE_TIME_OFFSET_INDICATOR, got 0x%04X", got)
	}
	if got := binary.BigEndian.Uint16(data[alternateOffset+2:]); got != 26 {
		t.Fatalf("alternate TLV length: want 26, got %d", got)
	}
}

func TestEncodeAnnounceC372382011ShortDisplayNameHasLength18(t *testing.T) {
	data := EncodeAnnounceC372382011(
		Header{MessageType: MsgAnnounce, VersionPTP: PTPVersion2},
		AnnounceBody{},
		C37238TLV2011{GrandmasterID: 3, GrandmasterTimeInaccuracy: 45},
		&AlternateTimeOffsetTLV{CurrentOffset: 10763, DisplayName: "LT"},
	)
	alternateOffset := HeaderSize + announceBodySize + 22
	if got := binary.BigEndian.Uint16(data[alternateOffset+2:]); got != 18 {
		t.Fatalf("alternate TLV length: want 18, got %d", got)
	}
	if got := data[alternateOffset+19]; got != 2 {
		t.Fatalf("displayName length: want 2, got %d", got)
	}
	if got := string(data[alternateOffset+20 : alternateOffset+22]); got != "LT" {
		t.Fatalf("displayName: want LT, got %q", got)
	}
}

func TestDecodeAnnounceProfileTLVs2011(t *testing.T) {
	wantC37 := C37238TLV2011{
		GrandmasterID:             3,
		GrandmasterTimeInaccuracy: 45,
		NetworkTimeInaccuracy:     7,
		Reserved:                  2,
	}
	wantAlternate := AlternateTimeOffsetTLV{
		KeyField:       1,
		CurrentOffset:  10763,
		JumpSeconds:    -1,
		TimeOfNextJump: 0x010203040506,
		DisplayName:    "UTC+03:00",
	}
	data := EncodeAnnounceC372382011(Header{}, AnnounceBody{}, wantC37, &wantAlternate)

	got, err := DecodeAnnounceProfileTLVs(data[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if got.C37238Version != C37238Version2011 || got.C372382011 == nil || *got.C372382011 != wantC37 {
		t.Fatalf("C37.238-2011 TLV: want %+v, got %+v", wantC37, got)
	}
	if got.AlternateTimeOffset == nil || *got.AlternateTimeOffset != wantAlternate {
		t.Fatalf("alternate time offset: want %+v, got %+v", wantAlternate, got.AlternateTimeOffset)
	}
}

func TestDecodeAnnounceProfileTLVs2017(t *testing.T) {
	want := C37238TLV2017{
		GrandmasterID:       9,
		Reserved:            0x01020304,
		TotalTimeInaccuracy: 100,
		Reserved2:           5,
	}
	data := EncodeAnnounceC372382017(Header{}, AnnounceBody{}, want, nil)

	got, err := DecodeAnnounceProfileTLVs(data[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if got.C37238Version != C37238Version2017 || got.C372382017 == nil || *got.C372382017 != want {
		t.Fatalf("C37.238-2017 TLV: want %+v, got %+v", want, got)
	}
	if got.AlternateTimeOffset != nil {
		t.Fatalf("unexpected alternate time offset: %+v", got.AlternateTimeOffset)
	}
}

func TestDecodeAnnounceProfileTLVsRejectsMalformedTLV(t *testing.T) {
	data := EncodeAnnounceC372382011(Header{}, AnnounceBody{}, C37238TLV2011{}, nil)
	data = data[HeaderSize:]
	data[announceBodySize+2] = 0
	data[announceBodySize+3] = 19
	if _, err := DecodeAnnounceProfileTLVs(data); err == nil {
		t.Fatal("malformed C37.238 TLV was accepted")
	}
}

func TestEncodeSyncFollowUpRoundTrip(t *testing.T) {
	ci := ClockIdentity{1, 2, 3, 0xFF, 0xFE, 4, 5, 6}
	seq := uint16(42)

	syncHdr := Header{
		MessageType:        MsgSync,
		VersionPTP:         PTPVersion2,
		FlagField:          FlagTwoStep,
		SourcePortIdentity: PortIdentity{ClockIdentity: ci, PortNumber: 1},
		SequenceID:         seq,
		ControlField:       ControlSync,
	}
	syncData := EncodeSync(syncHdr, SyncBody{})

	gotSyncHdr, err := DecodeHeader(syncData)
	if err != nil {
		t.Fatal(err)
	}
	if gotSyncHdr.FlagField&FlagTwoStep == 0 {
		t.Error("SYNC should have TWO_STEP flag")
	}

	txTime := time.Date(2025, 6, 1, 12, 0, 0, 500000000, time.UTC)
	fuHdr := Header{
		MessageType:        MsgFollowUp,
		VersionPTP:         PTPVersion2,
		SourcePortIdentity: PortIdentity{ClockIdentity: ci, PortNumber: 1},
		SequenceID:         seq,
		ControlField:       ControlFollowUp,
	}
	fuData := EncodeFollowUp(fuHdr, FollowUpBody{PreciseOriginTimestamp: TimestampFromTime(txTime)})
	if len(fuData) != HeaderSize+10 {
		t.Fatalf("FOLLOW_UP length: want %d, got %d", HeaderSize+10, len(fuData))
	}

	gotFuHdr, err := DecodeHeader(fuData)
	if err != nil {
		t.Fatal(err)
	}
	if gotFuHdr.SequenceID != seq {
		t.Errorf("FOLLOW_UP SequenceID: want %d, got %d", seq, gotFuHdr.SequenceID)
	}
	if int(gotFuHdr.MessageLength) != len(fuData) {
		t.Errorf("FOLLOW_UP messageLength: want %d, got %d", len(fuData), gotFuHdr.MessageLength)
	}
	fuBody, err := DecodeFollowUpBody(fuData[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	gotTime := fuBody.PreciseOriginTimestamp.ToTime()
	if !gotTime.Equal(txTime) {
		t.Errorf("PreciseOriginTimestamp: want %v, got %v", txTime, gotTime)
	}
}

func TestEncodeDelayRespRoundTrip(t *testing.T) {
	reqPI := PortIdentity{ClockIdentity: ClockIdentity{8, 7, 6, 5, 4, 3, 2, 1}, PortNumber: 3}
	rxTime := TimestampFromTime(time.Date(2025, 6, 1, 12, 0, 1, 0, time.UTC))
	corrField := int64(12345 << 16)

	hdr := Header{
		MessageType:        MsgDelayResp,
		VersionPTP:         PTPVersion2,
		CorrectionField:    corrField,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{1, 2, 3, 0xFF, 0xFE, 4, 5, 6}, PortNumber: 1},
		SequenceID:         99,
		ControlField:       ControlDelayResp,
	}
	body := DelayRespBody{
		ReceiveTimestamp:       rxTime,
		RequestingPortIdentity: reqPI,
	}
	data := EncodeDelayResp(hdr, body)

	gotHdr, err := DecodeHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.CorrectionField != corrField {
		t.Errorf("CorrectionField: want %d, got %d", corrField, gotHdr.CorrectionField)
	}

	gotBody, err := DecodeDelayRespBody(data[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.ReceiveTimestamp != rxTime {
		t.Errorf("ReceiveTimestamp: want %v, got %v", rxTime, gotBody.ReceiveTimestamp)
	}
	if gotBody.RequestingPortIdentity != reqPI {
		t.Errorf("RequestingPortIdentity: want %v, got %v", reqPI, gotBody.RequestingPortIdentity)
	}
}

func TestEncodePDelayRespRoundTrip(t *testing.T) {
	reqPI := PortIdentity{ClockIdentity: ClockIdentity{0xAA, 0xBB, 0xCC, 0xFF, 0xFE, 0x11, 0x22, 0x33}, PortNumber: 1}
	t2 := TimestampFromTime(time.Date(2025, 6, 1, 12, 0, 0, 100000000, time.UTC))

	respHdr := Header{
		MessageType:        MsgPDelayResp,
		VersionPTP:         PTPVersion2,
		FlagField:          FlagTwoStep,
		SourcePortIdentity: PortIdentity{ClockIdentity: ClockIdentity{1, 2, 3, 0xFF, 0xFE, 4, 5, 6}, PortNumber: 1},
		SequenceID:         77,
		ControlField:       ControlOther,
	}
	respBody := PDelayRespBody{
		RequestReceiptTimestamp: t2,
		RequestingPortIdentity:  reqPI,
	}
	respData := EncodePDelayResp(respHdr, respBody)

	gotHdr, err := DecodeHeader(respData)
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr.FlagField&FlagTwoStep == 0 {
		t.Error("PDELAY_RESP should have TWO_STEP flag")
	}

	gotBody, err := DecodePDelayRespBody(respData[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.RequestReceiptTimestamp != t2 {
		t.Errorf("RequestReceiptTimestamp: want %v, got %v", t2, gotBody.RequestReceiptTimestamp)
	}
	if gotBody.RequestingPortIdentity != reqPI {
		t.Errorf("RequestingPortIdentity: want %v, got %v", reqPI, gotBody.RequestingPortIdentity)
	}

	// PDELAY_RESP_FOLLOW_UP
	t3 := TimestampFromTime(time.Date(2025, 6, 1, 12, 0, 0, 200000000, time.UTC))
	fuHdr := Header{
		MessageType:        MsgPDelayRespFollowUp,
		VersionPTP:         PTPVersion2,
		CorrectionField:    555 << 16,
		SourcePortIdentity: respHdr.SourcePortIdentity,
		SequenceID:         77,
		ControlField:       ControlOther,
	}
	fuBody := PDelayRespFollowUpBody{
		ResponseOriginTimestamp: t3,
		RequestingPortIdentity:  reqPI,
	}
	fuData := EncodePDelayRespFollowUp(fuHdr, fuBody)

	gotFuHdr, err := DecodeHeader(fuData)
	if err != nil {
		t.Fatal(err)
	}
	if gotFuHdr.CorrectionField != 555<<16 {
		t.Errorf("CorrectionField: want %d, got %d", 555<<16, gotFuHdr.CorrectionField)
	}

	gotFuBody, err := DecodePDelayRespFollowUpBody(fuData[HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if gotFuBody.ResponseOriginTimestamp != t3 {
		t.Errorf("ResponseOriginTimestamp: want %v, got %v", t3, gotFuBody.ResponseOriginTimestamp)
	}
}

func TestDecodeDelayReqBody(t *testing.T) {
	ts := Timestamp{Seconds: 1710505845, Nanoseconds: 999}
	buf := make([]byte, timestampSize)
	encodeTimestamp(ts, buf)
	got, err := DecodeDelayReqBody(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginTimestamp != ts {
		t.Errorf("want %v, got %v", ts, got.OriginTimestamp)
	}
}

func TestDecodePDelayReqBody(t *testing.T) {
	ts := Timestamp{Seconds: 1710505845, Nanoseconds: 123}
	buf := make([]byte, timestampSize+10) // +10 reserved
	encodeTimestamp(ts, buf)
	got, err := DecodePDelayReqBody(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginTimestamp != ts {
		t.Errorf("want %v, got %v", ts, got.OriginTimestamp)
	}
}
