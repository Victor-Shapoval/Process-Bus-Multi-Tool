package ptpclient

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"pbmt/internal/application/ptpport"
	"pbmt/internal/domain/ptp"
)

type fakeClock struct {
	now         time.Time
	mapOffset   time.Duration
	steps       []time.Duration
	frequencies []float64
}

func (c *fakeClock) Now() time.Time                   { return c.now }
func (c *fakeClock) FromSystem(t time.Time) time.Time { return t.Add(c.mapOffset) }
func (c *fakeClock) Step(offset time.Duration) error {
	c.steps = append(c.steps, offset)
	return nil
}
func (c *fakeClock) AdjustFrequency(ppb float64) error {
	c.frequencies = append(c.frequencies, ppb)
	return nil
}
func (c *fakeClock) MaxFreqAdj() float64 { return 900000 }

type fakeTransport struct {
	txTime        time.Time
	timestampMode string
	eventCh       chan ptpport.Packet
	generalCh     chan ptpport.Packet
	errors        chan error
	eventSent     [][]byte
	generalSent   [][]byte
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		txTime:        time.Date(2026, 1, 1, 0, 0, 0, 100, time.UTC),
		timestampMode: "software",
		eventCh:       make(chan ptpport.Packet, 1),
		generalCh:     make(chan ptpport.Packet, 1),
		errors:        make(chan error, 1),
	}
}

func (t *fakeTransport) Start() {}
func (t *fakeTransport) Close() {}
func (t *fakeTransport) SendEvent(data []byte) (time.Time, error) {
	t.eventSent = append(t.eventSent, append([]byte(nil), data...))
	return t.txTime, nil
}
func (t *fakeTransport) SendGeneral(data []byte) error {
	t.generalSent = append(t.generalSent, append([]byte(nil), data...))
	return nil
}
func (t *fakeTransport) SendEventTo(data []byte, _ net.Addr) (time.Time, error) {
	return t.SendEvent(data)
}
func (t *fakeTransport) SendGeneralTo(data []byte, _ net.Addr) error {
	return t.SendGeneral(data)
}
func (t *fakeTransport) EventCh() <-chan ptpport.Packet   { return t.eventCh }
func (t *fakeTransport) GeneralCh() <-chan ptpport.Packet { return t.generalCh }
func (t *fakeTransport) Errors() <-chan error             { return t.errors }
func (t *fakeTransport) TimestampMode() string            { return t.timestampMode }

func testService() *Service {
	self := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentity{0xAA, 0, 0, 0xFF, 0xFE, 0, 0, 1},
		PortNumber:    1,
	}
	return New(newFakeTransport(), &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, self, Config{
		Profile:       ptp.DefaultProfile,
		Transport:     "udp",
		TimestampMode: "software",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestPowerProfileAlwaysUsesP2P(t *testing.T) {
	self := ptp.PortIdentity{ClockIdentity: ptp.ClockIdentity{1}, PortNumber: 1}
	svc := New(newFakeTransport(), &fakeClock{}, self, Config{
		Profile:        ptp.PowerProfile,
		DelayMechanism: ptp.DelayMechanismE2E,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if svc.profile.DelayMechanism != ptp.DelayMechanismP2P {
		t.Fatalf("Power Profile mechanism: want P2P, got %s", ptp.DelayMechanismName(svc.profile.DelayMechanism))
	}
}

func masterID(id byte) ptp.PortIdentity {
	return ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentity{id, 0, 0, 0xFF, 0xFE, 0, 0, 1},
		PortNumber:    1,
	}
}

func announcePayload(h ptp.Header, a ptp.AnnounceBody) []byte {
	data := ptp.EncodeAnnounce(h, a)
	return data[ptp.HeaderSize:]
}

func testPowerService(version ptp.C37238Version) *Service {
	profile := ptp.PowerProfile
	profile.C37238Version = version
	self := ptp.PortIdentity{ClockIdentity: ptp.ClockIdentity{0xAA}, PortNumber: 1}
	svc := New(newFakeTransport(), &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, self, Config{
		Profile:       profile,
		Transport:     "ethernet",
		TimestampMode: "software",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateListening))
	return svc
}

func powerAnnouncePayload(version ptp.C37238Version, h ptp.Header, a ptp.AnnounceBody, alternate *ptp.AlternateTimeOffsetTLV) []byte {
	var data []byte
	if version == ptp.C37238Version2017 {
		data = ptp.EncodeAnnounceC372382017(h, a, ptp.C37238TLV2017{
			GrandmasterID:       3,
			TotalTimeInaccuracy: 100,
		}, alternate)
	} else {
		data = ptp.EncodeAnnounceC372382011(h, a, ptp.C37238TLV2011{
			GrandmasterID:             3,
			GrandmasterTimeInaccuracy: 60,
			NetworkTimeInaccuracy:     5,
		}, alternate)
	}
	return data[ptp.HeaderSize:]
}

const absoluteUTCAnnounceFlags = ptp.FlagPTPTimescale | ptp.FlagCurrentUtcOffsetValid

func TestPowerProfileQualifiesOnlyMatchingAnnounceTLV(t *testing.T) {
	for _, version := range []ptp.C37238Version{ptp.C37238Version2011, ptp.C37238Version2017} {
		t.Run(version.String(), func(t *testing.T) {
			svc := testPowerService(version)
			master := masterID(1)
			hdr := ptp.Header{SourcePortIdentity: master, SequenceID: 10, FlagField: absoluteUTCAnnounceFlags}
			ann := ptp.AnnounceBody{
				CurrentUtcOffset:        37,
				GrandmasterPriority1:    100,
				GrandmasterPriority2:    128,
				GrandmasterIdentity:     master.ClockIdentity,
				GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
			}
			alternate := &ptp.AlternateTimeOffsetTLV{KeyField: 1, CurrentOffset: 10763, DisplayName: "UTC+03:00"}
			body := powerAnnouncePayload(version, hdr, ann, alternate)

			svc.handleAnnounce(hdr, body)
			hdr.SequenceID++
			svc.handleAnnounce(hdr, body)

			if svc.bestMaster == nil || svc.State() != StateUncalibrated {
				t.Fatalf("matching Power Profile master was not selected: master=%+v state=%s", svc.bestMaster, svc.State())
			}
			snapshot := svc.Snapshot()
			if snapshot.ExpectedC37238Version != version || snapshot.MasterC37238Version != version {
				t.Fatalf("C37.238 versions: expected=%s master=%s", snapshot.ExpectedC37238Version, snapshot.MasterC37238Version)
			}
			if snapshot.C37238GrandmasterID != 3 || snapshot.AlternateTimeOffset == nil || snapshot.AlternateTimeOffset.DisplayName != "UTC+03:00" {
				t.Fatalf("Power Profile snapshot is incomplete: %+v", snapshot)
			}
		})
	}
}

func TestPowerProfileRejectsMissingOrMismatchedAnnounceTLV(t *testing.T) {
	master := masterID(1)
	hdr := ptp.Header{SourcePortIdentity: master, SequenceID: 10, FlagField: absoluteUTCAnnounceFlags}
	ann := ptp.AnnounceBody{
		CurrentUtcOffset:        37,
		GrandmasterPriority1:    100,
		GrandmasterPriority2:    128,
		GrandmasterIdentity:     master.ClockIdentity,
		GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "missing", body: announcePayload(hdr, ann)},
		{name: "mismatched", body: powerAnnouncePayload(ptp.C37238Version2017, hdr, ann, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := testPowerService(ptp.C37238Version2011)
			svc.handleAnnounce(hdr, tc.body)
			hdr.SequenceID++
			svc.handleAnnounce(hdr, tc.body)
			if svc.bestMaster != nil || len(svc.foreignMasters) != 0 || svc.State() != StateListening {
				t.Fatalf("incompatible master was accepted: best=%+v table=%+v state=%s", svc.bestMaster, svc.foreignMasters, svc.State())
			}
		})
	}
}

func TestPowerProfileClientRespondsToPDelayReq(t *testing.T) {
	svc := testPowerService(ptp.C37238Version2011)
	tr := svc.tr.(*fakeTransport)
	svc.utcOffset = 37
	requester := masterID(4)
	requestHeader := ptp.Header{
		MessageType:        ptp.MsgPDelayReq,
		TransportSpecific:  svc.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       svc.profile.DomainNumber,
		CorrectionField:    3 << 16,
		SourcePortIdentity: requester,
		SequenceID:         55,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	requestData := ptp.EncodePDelayReq(requestHeader, ptp.PDelayReqBody{})
	rxTime := time.Date(2026, 1, 1, 12, 0, 0, 1234, time.UTC)
	svc.handleEvent(ptpport.Packet{Data: requestData, Timestamp: rxTime})

	if len(tr.eventSent) != 1 || len(tr.generalSent) != 1 {
		t.Fatalf("PDelay response pair: event=%d general=%d", len(tr.eventSent), len(tr.generalSent))
	}
	respHeader, err := ptp.DecodeHeader(tr.eventSent[0])
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ptp.DecodePDelayRespBody(tr.eventSent[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if respHeader.MessageType != ptp.MsgPDelayResp || respHeader.SequenceID != requestHeader.SequenceID || respHeader.FlagField&ptp.FlagTwoStep == 0 {
		t.Fatalf("PDelayResp header does not match request: %+v", respHeader)
	}
	if resp.RequestingPortIdentity != requester {
		t.Fatalf("PDelayResp requesting identity: want %s, got %s", requester, resp.RequestingPortIdentity)
	}
	if got := resp.RequestReceiptTimestamp.ToTimeWithUTCOffset(37); !got.Equal(rxTime) {
		t.Fatalf("PDelayResp receive timestamp: want %v, got %v", rxTime, got)
	}

	followUpHeader, err := ptp.DecodeHeader(tr.generalSent[0])
	if err != nil {
		t.Fatal(err)
	}
	followUp, err := ptp.DecodePDelayRespFollowUpBody(tr.generalSent[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if followUpHeader.MessageType != ptp.MsgPDelayRespFollowUp || followUpHeader.SequenceID != requestHeader.SequenceID || followUpHeader.CorrectionField != requestHeader.CorrectionField {
		t.Fatalf("PDelayRespFollowUp header does not match request: %+v", followUpHeader)
	}
	if followUp.RequestingPortIdentity != requester {
		t.Fatalf("PDelayRespFollowUp requesting identity: want %s, got %s", requester, followUp.RequestingPortIdentity)
	}
	if got := followUp.ResponseOriginTimestamp.ToTimeWithUTCOffset(37); !got.Equal(tr.txTime) {
		t.Fatalf("PDelayRespFollowUp egress timestamp: want %v, got %v", tr.txTime, got)
	}
	if snapshot := svc.Snapshot(); snapshot.PDelayRespCount != 1 {
		t.Fatalf("PDelay response count: want 1, got %d", snapshot.PDelayRespCount)
	}
}

func TestUTCOffsetTracksSelectedMasterOnly(t *testing.T) {
	svc := testService()
	goodMaster := masterID(1)
	badMaster := masterID(2)

	goodHeader := ptp.Header{
		SourcePortIdentity: goodMaster,
		FlagField:          ptp.FlagPTPTimescale | ptp.FlagCurrentUtcOffsetValid,
	}
	goodBody := announcePayload(ptp.Header{}, ptp.AnnounceBody{
		CurrentUtcOffset:        37,
		GrandmasterPriority1:    100,
		GrandmasterPriority2:    128,
		GrandmasterIdentity:     goodMaster.ClockIdentity,
		GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
	})
	svc.handleAnnounce(goodHeader, goodBody)
	goodHeader.SequenceID++
	svc.handleAnnounce(goodHeader, goodBody)

	badHeader := ptp.Header{
		SourcePortIdentity: badMaster,
		FlagField:          ptp.FlagPTPTimescale | ptp.FlagCurrentUtcOffsetValid,
	}
	badBody := announcePayload(ptp.Header{}, ptp.AnnounceBody{
		CurrentUtcOffset:        0,
		GrandmasterPriority1:    200,
		GrandmasterPriority2:    128,
		GrandmasterIdentity:     badMaster.ClockIdentity,
		GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
	})
	svc.handleAnnounce(badHeader, badBody)
	badHeader.SequenceID++
	svc.handleAnnounce(badHeader, badBody)

	utc := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := svc.ptpTimestampToTime(ptp.TimestampFromTimeWithUTCOffset(utc, 37))
	if !got.Equal(utc) {
		t.Fatalf("selected master UTC offset not used: want %v, got %v", utc, got)
	}
}

func TestAnnounceWithoutAbsoluteUTCFlagsDoesNotQualifyMaster(t *testing.T) {
	tests := []struct {
		name  string
		flags uint16
	}{
		{name: "arbitrary timescale", flags: ptp.FlagCurrentUtcOffsetValid},
		{name: "unknown UTC offset", flags: ptp.FlagPTPTimescale},
		{name: "both flags missing", flags: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := testService()
			svc.state.Store(uint32(StateListening))
			master := masterID(1)
			hdr := ptp.Header{
				SourcePortIdentity: master,
				SequenceID:         10,
				FlagField:          tt.flags,
			}
			body := announcePayload(ptp.Header{}, ptp.AnnounceBody{
				CurrentUtcOffset:        37,
				GrandmasterPriority1:    100,
				GrandmasterPriority2:    128,
				GrandmasterIdentity:     master.ClockIdentity,
				GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
			})

			svc.handleAnnounce(hdr, body)
			hdr.SequenceID++
			svc.handleAnnounce(hdr, body)

			if svc.bestMaster != nil {
				t.Fatalf("master without absolute UTC flags was selected: %+v", svc.bestMaster)
			}
			if len(svc.foreignMasters) != 0 {
				t.Fatalf("master without absolute UTC flags entered foreign master table: %+v", svc.foreignMasters)
			}
			if svc.State() != StateListening {
				t.Fatalf("state: want LISTENING, got %s", svc.State())
			}
		})
	}
}

func TestBuiltInProfilesAdvertiseAbsoluteUTC(t *testing.T) {
	for _, profile := range []ptp.Profile{ptp.DefaultProfile, ptp.PowerProfile} {
		t.Run(profile.Name, func(t *testing.T) {
			if !announceProvidesAbsoluteUTC(ptp.Header{FlagField: profile.FlagField}) {
				t.Fatalf("profile %q would be rejected by the UTC-only client: flags=0x%04X", profile.Name, profile.FlagField)
			}
		})
	}
}

func TestSelectedMasterIsRejectedWhenAbsoluteUTCFlagsDisappear(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateListening))
	master := masterID(1)
	hdr := ptp.Header{
		SourcePortIdentity: master,
		SequenceID:         10,
		FlagField:          absoluteUTCAnnounceFlags,
	}
	body := announcePayload(ptp.Header{}, ptp.AnnounceBody{
		CurrentUtcOffset:        37,
		GrandmasterPriority1:    100,
		GrandmasterPriority2:    128,
		GrandmasterIdentity:     master.ClockIdentity,
		GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
	})

	svc.handleAnnounce(hdr, body)
	hdr.SequenceID++
	svc.handleAnnounce(hdr, body)
	if svc.bestMaster == nil || svc.State() != StateUncalibrated {
		t.Fatalf("valid master was not selected: master=%+v state=%s", svc.bestMaster, svc.State())
	}

	hdr.SequenceID++
	hdr.FlagField = ptp.FlagPTPTimescale
	svc.handleAnnounce(hdr, body)

	if svc.bestMaster != nil || len(svc.foreignMasters) != 0 {
		t.Fatalf("master remained selected after UTC offset became invalid: best=%+v table=%+v", svc.bestMaster, svc.foreignMasters)
	}
	if svc.State() != StateListening {
		t.Fatalf("state: want LISTENING after rejecting selected master, got %s", svc.State())
	}
}

func TestForeignMasterRequiresTwoDistinctAnnounces(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateListening))
	master := masterID(1)
	hdr := ptp.Header{SourcePortIdentity: master, SequenceID: 10, FlagField: absoluteUTCAnnounceFlags}
	body := announcePayload(ptp.Header{}, ptp.AnnounceBody{
		GrandmasterPriority1:    100,
		GrandmasterPriority2:    128,
		GrandmasterIdentity:     master.ClockIdentity,
		GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: ptp.ClockAccuracy100ns},
	})

	svc.handleAnnounce(hdr, body)
	if svc.bestMaster != nil || svc.State() != StateListening {
		t.Fatal("foreign master was selected before qualification")
	}
	svc.handleAnnounce(hdr, body)
	if svc.bestMaster != nil {
		t.Fatal("duplicate Announce qualified a foreign master")
	}
	hdr.SequenceID++
	svc.handleAnnounce(hdr, body)
	if svc.bestMaster == nil || svc.bestMaster.Identity != master {
		t.Fatal("foreign master was not selected after two distinct Announces")
	}
	if svc.State() != StateUncalibrated {
		t.Fatalf("state: want UNCALIBRATED, got %s", svc.State())
	}
}

func TestAnnounceTimeoutFailsOverToQualifiedBackup(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateSlave))
	primaryID := masterID(1)
	backupID := masterID(2)
	primary := &ForeignMaster{
		Identity:      primaryID,
		Header:        ptp.Header{SourcePortIdentity: primaryID},
		Announce:      ptp.AnnounceBody{GrandmasterPriority1: 100, GrandmasterIdentity: primaryID.ClockIdentity},
		LastSeen:      time.Now().Add(-7 * time.Second),
		AnnounceCount: foreignMasterThreshold,
	}
	backup := &ForeignMaster{
		Identity:      backupID,
		Header:        ptp.Header{SourcePortIdentity: backupID},
		Announce:      ptp.AnnounceBody{GrandmasterPriority1: 200, GrandmasterIdentity: backupID.ClockIdentity},
		LastSeen:      time.Now(),
		AnnounceCount: foreignMasterThreshold,
	}
	svc.foreignMasters[primaryID] = primary
	svc.foreignMasters[backupID] = backup
	svc.bestMaster = primary

	svc.checkTimeouts()
	if svc.bestMaster != backup {
		t.Fatalf("best master: want backup %s, got %+v", backupID, svc.bestMaster)
	}
	if svc.State() != StateUncalibrated {
		t.Fatalf("state after failover: want UNCALIBRATED, got %s", svc.State())
	}
}

func TestDelayRequestsRejectEmptyTransportTimestamp(t *testing.T) {
	t.Run("E2E", func(t *testing.T) {
		svc := testService()
		svc.state.Store(uint32(StateUncalibrated))
		svc.tr.(*fakeTransport).txTime = time.Time{}
		svc.sendDelayReq()
		if svc.delayReqPending || svc.delayReqSeq != 0 {
			t.Fatalf("empty timestamp created a pending DelayReq: pending=%v seq=%d", svc.delayReqPending, svc.delayReqSeq)
		}
	})

	t.Run("P2P", func(t *testing.T) {
		svc := testService()
		svc.state.Store(uint32(StateUncalibrated))
		svc.profile.DelayMechanism = ptp.DelayMechanismP2P
		svc.tr.(*fakeTransport).txTime = time.Time{}
		svc.sendPDelayReq()
		if svc.pdelayReqPending || svc.pdelayReqSeq != 0 {
			t.Fatalf("empty timestamp created a pending PDelayReq: pending=%v seq=%d", svc.pdelayReqPending, svc.pdelayReqSeq)
		}
	})
}

func TestDelayRespRequiresOutstandingSequence(t *testing.T) {
	svc := testService()
	master := masterID(1)
	svc.state.Store(uint32(StateSlave))
	svc.bestMaster = &ForeignMaster{Identity: master}

	svc.sendDelayReq()
	respBody := ptp.DelayRespBody{
		ReceiveTimestamp:       ptp.TimestampFromTime(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)),
		RequestingPortIdentity: svc.selfID,
	}

	wrongHdr := ptp.Header{SourcePortIdentity: master, SequenceID: svc.lastDelayReqSeq + 1}
	svc.handleDelayResp(wrongHdr, ptp.EncodeDelayResp(wrongHdr, respBody)[ptp.HeaderSize:])
	if !svc.tsproc.t4.IsZero() {
		t.Fatal("stale delay response updated t4")
	}

	okHdr := ptp.Header{SourcePortIdentity: master, SequenceID: svc.lastDelayReqSeq}
	svc.handleDelayResp(okHdr, ptp.EncodeDelayResp(okHdr, respBody)[ptp.HeaderSize:])
	if svc.tsproc.t4.IsZero() {
		t.Fatal("matching delay response did not update t4")
	}
}

func TestPDelayRespRequiresOutstandingSequence(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateSlave))
	svc.profile.DelayMechanism = ptp.DelayMechanismP2P
	svc.sendPDelayReq()

	respBody := ptp.PDelayRespBody{
		RequestReceiptTimestamp: ptp.TimestampFromTime(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)),
		RequestingPortIdentity:  svc.selfID,
	}
	wrongHdr := ptp.Header{SequenceID: svc.lastPDelayReqSeq + 1, FlagField: ptp.FlagTwoStep}
	svc.handlePDelayResp(wrongHdr, ptp.EncodePDelayResp(wrongHdr, respBody)[ptp.HeaderSize:], time.Now())
	if !svc.pdelayT4.IsZero() {
		t.Fatal("stale pdelay response updated pdelay state")
	}

	okHdr := ptp.Header{SequenceID: svc.lastPDelayReqSeq, FlagField: ptp.FlagTwoStep}
	svc.handlePDelayResp(okHdr, ptp.EncodePDelayResp(okHdr, respBody)[ptp.HeaderSize:], time.Now())
	if svc.pdelayT4.IsZero() {
		t.Fatal("matching pdelay response did not update pdelay state")
	}

	fuBody := ptp.PDelayRespFollowUpBody{
		ResponseOriginTimestamp: ptp.TimestampFromTime(time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)),
		RequestingPortIdentity:  svc.selfID,
	}
	staleFUHdr := ptp.Header{SequenceID: svc.lastPDelayReqSeq + 1}
	svc.handlePDelayRespFollowUp(staleFUHdr, ptp.EncodePDelayRespFollowUp(staleFUHdr, fuBody)[ptp.HeaderSize:])
	if !svc.pdelayT3.IsZero() {
		t.Fatal("stale pdelay follow_up updated pdelay state")
	}

	okFUHdr := ptp.Header{SequenceID: svc.lastPDelayReqSeq}
	svc.handlePDelayRespFollowUp(okFUHdr, ptp.EncodePDelayRespFollowUp(okFUHdr, fuBody)[ptp.HeaderSize:])
	if svc.pdelayT3.IsZero() {
		t.Fatal("matching pdelay follow_up did not update pdelay state")
	}
}

func TestComputePeerDelayAppliesCorrectionBeforeHalving(t *testing.T) {
	svc := testService()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.pdelayT1 = base
	svc.pdelayT2 = base.Add(10 * time.Millisecond)
	svc.pdelayT3 = base.Add(12 * time.Millisecond)
	svc.pdelayT4 = base.Add(22 * time.Millisecond)

	svc.computePeerDelay(int64(2*time.Millisecond) << 16)
	if want := 9 * time.Millisecond; svc.tsproc.peerDelay != want {
		t.Fatalf("peer delay: want %v, got %v", want, svc.tsproc.peerDelay)
	}
}

func TestTwoStepPDelaySumsResponseAndFollowUpCorrections(t *testing.T) {
	svc := testService()
	svc.profile.DelayMechanism = ptp.DelayMechanismP2P
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.tsproc.SetT3(base)
	svc.lastPDelayReqSeq = 7
	svc.pdelayReqPending = true

	respHeader := ptp.Header{
		SequenceID:      7,
		FlagField:       ptp.FlagTwoStep,
		CorrectionField: int64(time.Millisecond) << 16,
	}
	respBody := ptp.PDelayRespBody{
		RequestReceiptTimestamp: ptp.TimestampFromTime(base.Add(10 * time.Millisecond)),
		RequestingPortIdentity:  svc.selfID,
	}
	svc.handlePDelayResp(
		respHeader,
		ptp.EncodePDelayResp(respHeader, respBody)[ptp.HeaderSize:],
		base.Add(22*time.Millisecond),
	)

	followUpHeader := ptp.Header{SequenceID: 7, CorrectionField: int64(time.Millisecond) << 16}
	followUpBody := ptp.PDelayRespFollowUpBody{
		ResponseOriginTimestamp: ptp.TimestampFromTime(base.Add(12 * time.Millisecond)),
		RequestingPortIdentity:  svc.selfID,
	}
	svc.handlePDelayRespFollowUp(
		followUpHeader,
		ptp.EncodePDelayRespFollowUp(followUpHeader, followUpBody)[ptp.HeaderSize:],
	)

	if want := 9 * time.Millisecond; svc.tsproc.peerDelay != want {
		t.Fatalf("peer delay: want %v, got %v", want, svc.tsproc.peerDelay)
	}
}

func TestPDelayFollowUpBeforeResponseIsBufferedAndApplied(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateSlave))
	svc.profile.DelayMechanism = ptp.DelayMechanismP2P
	responder := masterID(3)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.tsproc.SetT3(base)
	svc.lastPDelayReqSeq = 0
	svc.pdelayReqPending = true

	followUpHeader := ptp.Header{
		SourcePortIdentity: responder,
		SequenceID:         0,
		CorrectionField:    int64(time.Millisecond) << 16,
	}
	followUpBody := ptp.PDelayRespFollowUpBody{
		ResponseOriginTimestamp: ptp.TimestampFromTime(base.Add(12 * time.Millisecond)),
		RequestingPortIdentity:  svc.selfID,
	}
	svc.handlePDelayRespFollowUp(
		followUpHeader,
		ptp.EncodePDelayRespFollowUp(followUpHeader, followUpBody)[ptp.HeaderSize:],
	)
	if len(svc.pendingPDelayFUs) != 1 || !svc.pdelayT3.IsZero() || !svc.pdelayReqPending {
		t.Fatalf("early PDelay FollowUp was not buffered: pending=%d t3=%v requestPending=%t",
			len(svc.pendingPDelayFUs), svc.pdelayT3, svc.pdelayReqPending)
	}

	responseHeader := ptp.Header{
		SourcePortIdentity: responder,
		SequenceID:         0,
		FlagField:          ptp.FlagTwoStep,
		CorrectionField:    int64(time.Millisecond) << 16,
	}
	responseBody := ptp.PDelayRespBody{
		RequestReceiptTimestamp: ptp.TimestampFromTime(base.Add(10 * time.Millisecond)),
		RequestingPortIdentity:  svc.selfID,
	}
	svc.handlePDelayResp(
		responseHeader,
		ptp.EncodePDelayResp(responseHeader, responseBody)[ptp.HeaderSize:],
		base.Add(22*time.Millisecond),
	)

	if len(svc.pendingPDelayFUs) != 0 || svc.pdelayReqPending {
		t.Fatalf("buffered PDelay FollowUp was not consumed: pending=%d requestPending=%t",
			len(svc.pendingPDelayFUs), svc.pdelayReqPending)
	}
	if want := 9 * time.Millisecond; svc.tsproc.peerDelay != want {
		t.Fatalf("peer delay after reordered response: want %v, got %v", want, svc.tsproc.peerDelay)
	}
}

func TestFollowUpBeforeSyncIsBufferedAndApplied(t *testing.T) {
	svc := testService()
	svc.state.Store(uint32(StateUncalibrated))
	master := masterID(1)
	svc.bestMaster = &ForeignMaster{Identity: master}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.tsproc.SetT3(base.Add(20 * time.Millisecond))
	svc.tsproc.SetT4(base.Add(30 * time.Millisecond))

	followUpHeader := ptp.Header{SourcePortIdentity: master, SequenceID: 42}
	followUpBody := ptp.FollowUpBody{PreciseOriginTimestamp: ptp.TimestampFromTime(base)}
	svc.handleFollowUp(
		followUpHeader,
		ptp.EncodeFollowUp(followUpHeader, followUpBody)[ptp.HeaderSize:],
	)
	if len(svc.pendingFollowUps) != 1 || !svc.tsproc.t1.IsZero() {
		t.Fatalf("early FollowUp was not buffered: pending=%d t1=%v", len(svc.pendingFollowUps), svc.tsproc.t1)
	}

	syncHeader := ptp.Header{
		SourcePortIdentity: master,
		SequenceID:         42,
		FlagField:          ptp.FlagTwoStep,
	}
	svc.handleSync(syncHeader, nil, base.Add(10*time.Millisecond))

	if len(svc.pendingFollowUps) != 0 {
		t.Fatalf("buffered FollowUp was not consumed: pending=%d", len(svc.pendingFollowUps))
	}
	if !svc.tsproc.t1.Equal(base) {
		t.Fatalf("precise origin timestamp: want %v, got %v", base, svc.tsproc.t1)
	}
	svc.statusMu.RLock()
	syncCount := svc.syncCount
	svc.statusMu.RUnlock()
	if syncCount != 1 || svc.syncReceived {
		t.Fatalf("reordered Sync exchange was not completed: count=%d syncReceived=%t", syncCount, svc.syncReceived)
	}
}

func TestPendingFollowUpBuffersAreBounded(t *testing.T) {
	svc := testService()
	source := masterID(1)
	total := pendingFollowUpLimit + 3
	for i := 0; i < total; i++ {
		header := ptp.Header{SourcePortIdentity: source, SequenceID: uint16(i)}
		svc.storePendingFollowUp(pendingFollowUp{header: header})
		svc.storePendingPDelayFollowUp(pendingPDelayFollowUp{header: header})
	}
	if len(svc.pendingFollowUps) != pendingFollowUpLimit || len(svc.pendingPDelayFUs) != pendingFollowUpLimit {
		t.Fatalf("pending buffers exceeded limit %d: sync=%d pdelay=%d",
			pendingFollowUpLimit, len(svc.pendingFollowUps), len(svc.pendingPDelayFUs))
	}
	wantOldest := uint16(total - pendingFollowUpLimit)
	if svc.pendingFollowUps[0].header.SequenceID != wantOldest || svc.pendingPDelayFUs[0].header.SequenceID != wantOldest {
		t.Fatalf("oldest entries were not evicted: want seq=%d, sync=%d pdelay=%d",
			wantOldest, svc.pendingFollowUps[0].header.SequenceID, svc.pendingPDelayFUs[0].header.SequenceID)
	}
}

func TestTransportTimestampsAreMappedToApplicationClock(t *testing.T) {
	clk := &fakeClock{
		now:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		mapOffset: 3 * time.Second,
	}
	tr := newFakeTransport()
	svc := New(tr, clk, masterID(9), Config{
		Profile:       ptp.DefaultProfile,
		Transport:     "udp",
		TimestampMode: "software",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateSlave))

	svc.sendDelayReq()
	if want := tr.txTime.Add(clk.mapOffset); !svc.tsproc.t3.Equal(want) {
		t.Fatalf("mapped TX timestamp: want %v, got %v", want, svc.tsproc.t3)
	}

	svc.bestMaster = &ForeignMaster{Identity: masterID(1)}
	rx := tr.txTime.Add(time.Second)
	hdr := ptp.Header{SourcePortIdentity: masterID(1), FlagField: ptp.FlagTwoStep}
	svc.handleSync(hdr, nil, rx)
	if want := rx.Add(clk.mapOffset); !svc.tsproc.t2.Equal(want) {
		t.Fatalf("mapped RX timestamp: want %v, got %v", want, svc.tsproc.t2)
	}
}

func TestSyncTimeoutMarksClientUncalibrated(t *testing.T) {
	svc := testService()
	master := masterID(1)
	fm := &ForeignMaster{Identity: master, LastSeen: time.Now(), AnnounceCount: foreignMasterThreshold}
	svc.bestMaster = fm
	svc.foreignMasters[master] = fm
	svc.state.Store(uint32(StateSlave))
	svc.statusMu.Lock()
	svc.lastSync = time.Now().Add(-4 * time.Second)
	svc.statusMu.Unlock()

	svc.checkTimeouts()
	if got := svc.State(); got != StateUncalibrated {
		t.Fatalf("state after Sync timeout: want UNCALIBRATED, got %s", got)
	}
}

func TestClockDisciplineAppliesOppositeCorrection(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newFakeTransport()
	tr.timestampMode = "hardware"
	svc := New(tr, clk, masterID(9), Config{
		Profile:       ptp.DefaultProfile,
		Transport:     "udp",
		TimestampMode: "hardware",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateSlave))

	// The first sample initializes the servo. The second one is far enough
	// apart to enter JUMP and must move an ahead local clock backwards.
	svc.disciplineClock(2 * time.Second)
	clk.now = clk.now.Add(2 * time.Second)
	svc.disciplineClock(2 * time.Second)
	if len(clk.steps) != 1 || clk.steps[0] != -2*time.Second {
		t.Fatalf("clock step: want -2s, got %v", clk.steps)
	}
}

func TestSuccessfulE2EStepInvalidatesOutstandingDelayExchange(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newFakeTransport()
	tr.timestampMode = "hardware"
	svc := New(tr, clk, masterID(9), Config{
		Profile:       ptp.DefaultProfile,
		Transport:     "udp",
		TimestampMode: "hardware",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateSlave))

	// The first sample initializes the servo. Install an E2E exchange made on
	// the old application-clock scale before the second sample triggers a step.
	svc.disciplineClock(2 * time.Second)
	base := clk.now
	svc.tsproc.SetT1(base)
	svc.tsproc.SetT2(base.Add(time.Millisecond))
	svc.tsproc.SetT3(base.Add(2 * time.Millisecond))
	svc.tsproc.SetT4(base.Add(3 * time.Millisecond))
	svc.tsproc.SetCorrectionSync(1 << 16)
	svc.tsproc.SetCorrectionFollowUp(2 << 16)
	svc.tsproc.SetCorrectionDelayResp(3 << 16)
	svc.delayReqPending = true

	clk.now = clk.now.Add(2 * time.Second)
	svc.disciplineClock(2 * time.Second)

	if len(clk.steps) != 1 {
		t.Fatalf("expected one successful clock step, got %v", clk.steps)
	}
	if !svc.tsproc.t3.IsZero() || !svc.tsproc.t4.IsZero() {
		t.Fatalf("pre-step E2E timestamps survived step: t3=%v t4=%v", svc.tsproc.t3, svc.tsproc.t4)
	}
	if svc.tsproc.correctionSync != 0 || svc.tsproc.correctionFollowUp != 0 || svc.tsproc.correctionDelayResp != 0 {
		t.Fatalf("pre-step corrections survived step: sync=%d follow_up=%d delay_resp=%d",
			svc.tsproc.correctionSync, svc.tsproc.correctionFollowUp, svc.tsproc.correctionDelayResp)
	}
	if svc.delayReqPending {
		t.Fatal("pre-step DelayReq remained pending after step")
	}
	if _, _, ok := svc.tsproc.OffsetE2E(); ok {
		t.Fatal("E2E offset was produced before a fresh DelayReq exchange")
	}
}

func TestP2PStepPreservesPeerDelay(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newFakeTransport()
	tr.timestampMode = "hardware"
	profile := ptp.PowerProfile
	svc := New(tr, clk, masterID(9), Config{
		Profile:       profile,
		Transport:     "ethernet",
		TimestampMode: "hardware",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateSlave))
	svc.tsproc.SetPeerDelay(250 * time.Microsecond)
	svc.pdelayReqPending = true

	svc.disciplineClock(2 * time.Second)
	clk.now = clk.now.Add(2 * time.Second)
	svc.disciplineClock(2 * time.Second)

	if len(clk.steps) != 1 {
		t.Fatalf("expected one successful clock step, got %v", clk.steps)
	}
	if got := svc.tsproc.peerDelay; got != 250*time.Microsecond {
		t.Fatalf("P2P peer delay changed across clock step: want 250us, got %v", got)
	}
	if svc.pdelayReqPending {
		t.Fatal("pre-step PDelay exchange remained pending after step")
	}
}

func TestClockDisciplineSlowsAheadClock(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := newFakeTransport()
	tr.timestampMode = "hardware"
	svc := New(tr, clk, masterID(9), Config{
		Profile:       ptp.DefaultProfile,
		Transport:     "udp",
		TimestampMode: "hardware",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.state.Store(uint32(StateSlave))

	for i := 0; i < 3; i++ {
		svc.disciplineClock(100 * time.Microsecond)
		clk.now = clk.now.Add(2 * time.Second)
	}
	if len(clk.frequencies) == 0 {
		t.Fatal("expected frequency correction")
	}
	if got := clk.frequencies[len(clk.frequencies)-1]; got >= 0 {
		t.Fatalf("ahead clock must be slowed down, got correction %f ppb", got)
	}
}
