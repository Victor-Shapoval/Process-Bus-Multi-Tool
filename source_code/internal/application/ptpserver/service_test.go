package ptpserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"pbmt/internal/application/ptpport"
	"pbmt/internal/domain/ptp"
)

// mockTransport implements ptpport.Transport for tests.
type mockTransport struct {
	mu            sync.Mutex
	events        [][]byte // sent event messages
	generals      [][]byte // sent general messages
	eventCh       chan ptpport.Packet
	genCh         chan ptpport.Packet
	errCh         chan error
	txTime        time.Time
	timestampMode string
	eventTo       []net.Addr
	generalTo     []net.Addr
}

type mockEthernetAddr string

type clockedMockTransport struct {
	*mockTransport
	now time.Time
	err error
}

type transportWithoutClock struct {
	ptpport.Transport
}

func (m *clockedMockTransport) Now() (time.Time, error) { return m.now, m.err }

func (a mockEthernetAddr) Network() string { return "ethernet" }
func (a mockEthernetAddr) String() string  { return string(a) }

func newMockTransport() *mockTransport {
	return &mockTransport{
		eventCh:       make(chan ptpport.Packet, 64),
		genCh:         make(chan ptpport.Packet, 64),
		errCh:         make(chan error, 1),
		txTime:        time.Date(2025, 6, 1, 12, 0, 0, 500000000, time.UTC),
		timestampMode: "hardware",
	}
}

func (m *mockTransport) Start()                           {}
func (m *mockTransport) Close()                           {}
func (m *mockTransport) EventCh() <-chan ptpport.Packet   { return m.eventCh }
func (m *mockTransport) GeneralCh() <-chan ptpport.Packet { return m.genCh }
func (m *mockTransport) Errors() <-chan error             { return m.errCh }
func (m *mockTransport) TimestampMode() string            { return m.timestampMode }
func (m *mockTransport) Now() (time.Time, error)          { return m.txTime, nil }

func (m *mockTransport) SendEvent(data []byte) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.events = append(m.events, cp)
	return m.txTime, nil
}

func (m *mockTransport) SendGeneral(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.generals = append(m.generals, cp)
	return nil
}

func (m *mockTransport) SendEventTo(data []byte, dst net.Addr) (time.Time, error) {
	m.mu.Lock()
	m.eventTo = append(m.eventTo, cloneMockAddr(dst))
	m.mu.Unlock()
	return m.SendEvent(data)
}

func (m *mockTransport) SendGeneralTo(data []byte, dst net.Addr) error {
	m.mu.Lock()
	m.generalTo = append(m.generalTo, cloneMockAddr(dst))
	m.mu.Unlock()
	return m.SendGeneral(data)
}

func cloneMockAddr(addr net.Addr) net.Addr {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok || udpAddr == nil {
		return addr
	}
	copyAddr := *udpAddr
	copyAddr.IP = append(net.IP(nil), udpAddr.IP...)
	return &copyAddr
}

func (m *mockTransport) sentEvents() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.events...)
}

func (m *mockTransport) sentGenerals() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.generals...)
}

func (m *mockTransport) unicastCounts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.eventTo), len(m.generalTo)
}

func (m *mockTransport) unicastDestinations() ([]net.Addr, []net.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]net.Addr(nil), m.eventTo...), append([]net.Addr(nil), m.generalTo...)
}

func testSelfID() ptp.PortIdentity {
	return ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0xAA, 0xBB, 0xCC, 0x11, 0x22, 0x33}),
		PortNumber:    1,
	}
}

func testConfig() Config {
	domainNumber := uint8(0)
	priority1 := uint8(128)
	priority2 := uint8(128)
	return Config{
		Profile:      ptp.DefaultProfile,
		Transport:    "udp",
		DomainNumber: &domainNumber,
		UTCOffset:    37,
		TimeSource:   ptp.TimeSourceGPS,
		Priority1:    &priority1,
		Priority2:    &priority2,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewInitializesState(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	if svc.State() != StateInitializing {
		t.Errorf("initial state: want INITIALIZING, got %s", svc.State())
	}
}

func TestRunRejectsSoftwareTimestampTransport(t *testing.T) {
	tr := newMockTransport()
	tr.timestampMode = "software"
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	err := svc.Run(context.Background())
	if err == nil {
		t.Fatal("expected hardware timestamping requirement error")
	}
	if svc.State() != StateFaulty {
		t.Fatalf("state: want FAULTY, got %s", svc.State())
	}
}

func TestRunRejectsHardwareTransportWithoutClock(t *testing.T) {
	tr := transportWithoutClock{Transport: newMockTransport()}
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	err := svc.Run(context.Background())
	if err == nil {
		t.Fatal("expected hardware transport clock requirement error")
	}
	if svc.State() != StateFaulty {
		t.Fatalf("state: want FAULTY, got %s", svc.State())
	}
}

func TestPowerProfileAlwaysUsesP2P(t *testing.T) {
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	cfg.DelayMechanism = ptp.DelayMechanismE2E
	svc := New(newMockTransport(), testSelfID(), cfg, testLogger())
	if svc.profile.DelayMechanism != ptp.DelayMechanismP2P {
		t.Fatalf("Power Profile mechanism: want P2P, got %s", ptp.DelayMechanismName(svc.profile.DelayMechanism))
	}
}

func TestNewAppliesDelayMechanismOverride(t *testing.T) {
	cfg := testConfig()
	cfg.DelayMechanism = ptp.DelayMechanismP2P
	svc := New(newMockTransport(), testSelfID(), cfg, testLogger())
	if got := svc.Snapshot().DelayMechanism; got != "P2P" {
		t.Fatalf("delay mechanism: want P2P, got %q", got)
	}
}

func TestNewAppliesClockVarianceOverride(t *testing.T) {
	cfg := testConfig()
	variance := uint16(0x1234)
	cfg.ClockVariance = &variance
	svc := New(newMockTransport(), testSelfID(), cfg, testLogger())
	if got := svc.Snapshot().GrandmasterClockVariance; got != variance {
		t.Fatalf("clock variance: want 0x%04X, got 0x%04X", variance, got)
	}
}

func TestSendAnnounce(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}

	generals := tr.sentGenerals()
	if len(generals) != 1 {
		t.Fatalf("expected 1 general message, got %d", len(generals))
	}

	hdr, err := ptp.DecodeHeader(generals[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.MessageType != ptp.MsgAnnounce {
		t.Errorf("MessageType: want ANNOUNCE, got %d", hdr.MessageType)
	}
	if hdr.SequenceID != 0 {
		t.Errorf("SequenceID: want 0, got %d", hdr.SequenceID)
	}
	if hdr.FlagField&ptp.FlagPTPTimescale == 0 {
		t.Error("ANNOUNCE should have PTP_TIMESCALE flag")
	}

	body, err := ptp.DecodeAnnounceBody(generals[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if body.CurrentUtcOffset != 37 {
		t.Errorf("CurrentUtcOffset: want 37, got %d", body.CurrentUtcOffset)
	}
	if body.GrandmasterClockQuality.ClockClass != ptp.ClockClass6 {
		t.Errorf("ClockClass: want %d, got %d", ptp.ClockClass6, body.GrandmasterClockQuality.ClockClass)
	}
	if body.GrandmasterClockQuality.ClockAccuracy != ptp.ClockAccuracy100ns {
		t.Errorf("ClockAccuracy: want 0x%02X, got 0x%02X", ptp.ClockAccuracy100ns, body.GrandmasterClockQuality.ClockAccuracy)
	}
	if body.GrandmasterClockQuality.OffsetScaledLogVariance != ptp.ClockVarianceGPS {
		t.Errorf("OffsetScaledLogVariance: want 0x%04X, got 0x%04X", ptp.ClockVarianceGPS, body.GrandmasterClockQuality.OffsetScaledLogVariance)
	}
	if body.TimeSource != ptp.TimeSourceGPS {
		t.Errorf("TimeSource: want 0x%02X, got 0x%02X", ptp.TimeSourceGPS, body.TimeSource)
	}
}

func TestSendAnnounceUsesTransportClock(t *testing.T) {
	clockNow := time.Date(2026, 8, 7, 12, 34, 56, 789, time.UTC)
	tr := &clockedMockTransport{mockTransport: newMockTransport(), now: clockNow}
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}
	data := tr.sentGenerals()[0]
	body, err := ptp.DecodeAnnounceBody(data[ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	want := ptp.TimestampFromTimeWithUTCOffset(clockNow, 37)
	if body.OriginTimestamp != want {
		t.Fatalf("Announce clock: want %+v, got %+v", want, body.OriginTimestamp)
	}
}

func TestSendAnnounceFailsWhenTransportClockFails(t *testing.T) {
	tr := &clockedMockTransport{mockTransport: newMockTransport(), err: errors.New("clock unavailable")}
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	if err := svc.sendAnnounce(); err == nil {
		t.Fatal("expected transport clock error")
	}
	if len(tr.sentGenerals()) != 0 {
		t.Fatal("Announce was sent without a valid Grandmaster clock")
	}
}

func TestTraceabilityRequiresExplicitOverride(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	svc := New(tr, testSelfID(), cfg, testLogger())
	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}
	hdr, err := ptp.DecodeHeader(tr.sentGenerals()[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.FlagField&(ptp.FlagTimeTraceable|ptp.FlagFrequencyTraceable) != 0 {
		t.Fatalf("default Announce has traceability flags: 0x%04x", hdr.FlagField)
	}

	traceable := true
	cfg.TimeTraceable = &traceable
	cfg.FrequencyTraceable = &traceable
	tr = newMockTransport()
	svc = New(tr, testSelfID(), cfg, testLogger())
	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}
	hdr, err = ptp.DecodeHeader(tr.sentGenerals()[0])
	if err != nil {
		t.Fatal(err)
	}
	want := ptp.FlagTimeTraceable | ptp.FlagFrequencyTraceable
	if hdr.FlagField&want != want {
		t.Fatalf("explicit traceability flags were not applied: 0x%04x", hdr.FlagField)
	}
}

func TestPDelayUnicastRequestProducesFlaggedUnicastReplies(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(254)
	cfg.DomainNumber = &domainNumber
	svc := New(tr, testSelfID(), cfg, testLogger())
	svc.state.Store(uint32(StateMaster))
	reqHdr := ptp.Header{
		MessageType:       ptp.MsgPDelayReq,
		TransportSpecific: 0,
		VersionPTP:        ptp.PTPVersion2,
		DomainNumber:      254,
		FlagField:         ptp.FlagUnicast,
		SourcePortIdentity: ptp.PortIdentity{
			ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{1, 2, 3, 4, 5, 6}),
			PortNumber:    1,
		},
	}
	svc.handleEventPacket(ptpport.Packet{
		Data:      ptp.EncodePDelayReq(reqHdr, ptp.PDelayReqBody{}),
		From:      &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: ptp.UDPEventPort},
		Timestamp: time.Now(),
	})
	if eventTo, generalTo := tr.unicastCounts(); eventTo != 1 || generalTo != 1 {
		t.Fatalf("unicast replies: want event=1 general=1, got event=%d general=%d", eventTo, generalTo)
	}
	for _, data := range append(tr.sentEvents(), tr.sentGenerals()...) {
		hdr, err := ptp.DecodeHeader(data)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.FlagField&ptp.FlagUnicast == 0 {
			t.Errorf("%s reply has no unicast flag", messageTypeName(hdr.MessageType))
		}
	}
}

func messageTypeName(messageType uint8) string {
	switch messageType {
	case ptp.MsgPDelayResp:
		return "PDELAY_RESP"
	case ptp.MsgPDelayRespFollowUp:
		return "PDELAY_RESP_FOLLOW_UP"
	default:
		return fmt.Sprintf("message type 0x%X", messageType)
	}
}

func TestMultipleE2EClientsReceiveIndependentResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unicast bool
	}{
		{name: "multicast"},
		{name: "unicast", unicast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMockTransport()
			svc := New(tr, testSelfID(), testConfig(), testLogger())
			svc.state.Store(uint32(StateMaster))

			requesters := []ptp.PortIdentity{
				{ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x10, 0, 0, 0, 0, 1}), PortNumber: 1},
				{ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x10, 0, 0, 0, 0, 2}), PortNumber: 1},
			}
			addresses := []*net.UDPAddr{
				{IP: net.IPv4(192, 0, 2, 11), Port: ptp.UDPEventPort},
				{IP: net.IPv4(192, 0, 2, 12), Port: ptp.UDPEventPort},
			}

			for i, requester := range requesters {
				flags := uint16(0)
				if tc.unicast {
					flags = ptp.FlagUnicast
				}
				hdr := ptp.Header{
					MessageType:        ptp.MsgDelayReq,
					VersionPTP:         ptp.PTPVersion2,
					FlagField:          flags,
					SourcePortIdentity: requester,
					SequenceID:         uint16(100 + i),
					ControlField:       ptp.ControlDelayReq,
				}
				svc.handleEventPacket(ptpport.Packet{
					Data:      ptp.EncodeDelayReq(hdr, ptp.DelayReqBody{}),
					From:      addresses[i],
					Timestamp: time.Date(2025, 6, 1, 12, 0, i, 0, time.UTC),
				})
			}

			responses := tr.sentGenerals()
			if len(responses) != len(requesters) {
				t.Fatalf("DELAY_RESP count: want %d, got %d", len(requesters), len(responses))
			}
			eventTo, generalTo := tr.unicastCounts()
			wantGeneralTo := 0
			if tc.unicast {
				wantGeneralTo = len(requesters)
			}
			if eventTo != 0 || generalTo != wantGeneralTo {
				t.Fatalf("unicast sends: want event=0 general=%d, got event=%d general=%d", wantGeneralTo, eventTo, generalTo)
			}

			_, destinations := tr.unicastDestinations()
			for i, data := range responses {
				hdr, err := ptp.DecodeHeader(data)
				if err != nil {
					t.Fatal(err)
				}
				body, err := ptp.DecodeDelayRespBody(data[ptp.HeaderSize:])
				if err != nil {
					t.Fatal(err)
				}
				if hdr.SequenceID != uint16(100+i) || body.RequestingPortIdentity != requesters[i] {
					t.Errorf("response %d does not match requester: header=%+v body=%+v", i, hdr, body)
				}
				if tc.unicast {
					if hdr.FlagField&ptp.FlagUnicast == 0 || hdr.LogMessageInterval != 0x7F {
						t.Errorf("unicast response %d has flags=0x%04X logInterval=%d", i, hdr.FlagField, hdr.LogMessageInterval)
					}
					if destinations[i].String() != addresses[i].String() {
						t.Errorf("unicast destination %d: want %s, got %s", i, addresses[i], destinations[i])
					}
				} else if hdr.FlagField&ptp.FlagUnicast != 0 || hdr.LogMessageInterval != svc.profile.LogDelayReqInterval {
					t.Errorf("multicast response %d has flags=0x%04X logInterval=%d", i, hdr.FlagField, hdr.LogMessageInterval)
				}
			}
			if got := svc.Snapshot().DelayRespCount; got != uint64(len(requesters)) {
				t.Fatalf("Delay response count: want %d, got %d", len(requesters), got)
			}
		})
	}
}

func TestMultiplePowerProfilePeersReceiveIndependentPDelayResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unicast bool
	}{
		{name: "multicast"},
		{name: "unicast", unicast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newMockTransport()
			cfg := testConfig()
			cfg.Profile = ptp.PowerProfile
			cfg.Transport = "ethernet"
			domainNumber := uint8(254)
			cfg.DomainNumber = &domainNumber
			svc := New(tr, testSelfID(), cfg, testLogger())
			svc.state.Store(uint32(StateMaster))

			requesters := []ptp.PortIdentity{
				{ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x20, 0, 0, 0, 0, 1}), PortNumber: 1},
				{ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x20, 0, 0, 0, 0, 2}), PortNumber: 1},
			}
			addresses := []net.Addr{
				mockEthernetAddr("02:00:00:00:00:21"),
				mockEthernetAddr("02:00:00:00:00:22"),
			}

			for i, requester := range requesters {
				flags := uint16(0)
				if tc.unicast {
					flags = ptp.FlagUnicast
				}
				hdr := ptp.Header{
					MessageType:        ptp.MsgPDelayReq,
					VersionPTP:         ptp.PTPVersion2,
					DomainNumber:       domainNumber,
					FlagField:          flags,
					CorrectionField:    int64(i+1) << 16,
					SourcePortIdentity: requester,
					SequenceID:         uint16(200 + i),
					ControlField:       ptp.ControlOther,
				}
				svc.handleEventPacket(ptpport.Packet{
					Data:      ptp.EncodePDelayReq(hdr, ptp.PDelayReqBody{}),
					From:      addresses[i],
					Timestamp: time.Date(2025, 6, 1, 12, 1, i, 0, time.UTC),
				})
			}

			events := tr.sentEvents()
			generals := tr.sentGenerals()
			if len(events) != len(requesters) || len(generals) != len(requesters) {
				t.Fatalf("PDelay response pairs: want %d, got event=%d general=%d", len(requesters), len(events), len(generals))
			}
			eventTo, generalTo := tr.unicastCounts()
			wantTo := 0
			if tc.unicast {
				wantTo = len(requesters)
			}
			if eventTo != wantTo || generalTo != wantTo {
				t.Fatalf("unicast sends: want event=%d general=%d, got event=%d general=%d", wantTo, wantTo, eventTo, generalTo)
			}
			eventDestinations, generalDestinations := tr.unicastDestinations()

			for i := range requesters {
				respHdr, err := ptp.DecodeHeader(events[i])
				if err != nil {
					t.Fatal(err)
				}
				resp, err := ptp.DecodePDelayRespBody(events[i][ptp.HeaderSize:])
				if err != nil {
					t.Fatal(err)
				}
				fuHdr, err := ptp.DecodeHeader(generals[i])
				if err != nil {
					t.Fatal(err)
				}
				fu, err := ptp.DecodePDelayRespFollowUpBody(generals[i][ptp.HeaderSize:])
				if err != nil {
					t.Fatal(err)
				}

				wantSeq := uint16(200 + i)
				if respHdr.SequenceID != wantSeq || fuHdr.SequenceID != wantSeq ||
					resp.RequestingPortIdentity != requesters[i] || fu.RequestingPortIdentity != requesters[i] {
					t.Errorf("response pair %d does not match requester: respHeader=%+v resp=%+v followUpHeader=%+v followUp=%+v", i, respHdr, resp, fuHdr, fu)
				}
				if respHdr.FlagField&ptp.FlagTwoStep == 0 {
					t.Errorf("PDELAY_RESP %d has no two-step flag", i)
				}
				if tc.unicast {
					if respHdr.FlagField&ptp.FlagUnicast == 0 || fuHdr.FlagField&ptp.FlagUnicast == 0 {
						t.Errorf("unicast response pair %d is not flagged as unicast", i)
					}
					if eventDestinations[i].String() != addresses[i].String() || generalDestinations[i].String() != addresses[i].String() {
						t.Errorf("unicast destinations %d: want %s, got event=%s general=%s", i, addresses[i], eventDestinations[i], generalDestinations[i])
					}
				} else if respHdr.FlagField&ptp.FlagUnicast != 0 || fuHdr.FlagField&ptp.FlagUnicast != 0 {
					t.Errorf("multicast response pair %d is flagged as unicast", i)
				}
			}
			if got := svc.Snapshot().PDelayRespCount; got != uint64(len(requesters)) {
				t.Fatalf("PDelay response count: want %d, got %d", len(requesters), got)
			}
		})
	}
}

func TestProfileDelayMechanismFiltersRequests(t *testing.T) {
	slaveID := ptp.PortIdentity{ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{1, 2, 3, 4, 5, 6}), PortNumber: 1}
	t.Run("E2E ignores PDelayReq", func(t *testing.T) {
		tr := newMockTransport()
		svc := New(tr, testSelfID(), testConfig(), testLogger())
		svc.state.Store(uint32(StateMaster))
		hdr := ptp.Header{MessageType: ptp.MsgPDelayReq, VersionPTP: ptp.PTPVersion2, SourcePortIdentity: slaveID}
		svc.handleEventPacket(ptpport.Packet{Data: ptp.EncodePDelayReq(hdr, ptp.PDelayReqBody{}), Timestamp: time.Now()})
		if len(tr.sentEvents()) != 0 || len(tr.sentGenerals()) != 0 {
			t.Fatal("E2E profile answered PDelayReq")
		}
	})
	t.Run("P2P ignores DelayReq", func(t *testing.T) {
		tr := newMockTransport()
		cfg := testConfig()
		cfg.Profile = ptp.PowerProfile
		domainNumber := uint8(254)
		cfg.DomainNumber = &domainNumber
		svc := New(tr, testSelfID(), cfg, testLogger())
		svc.state.Store(uint32(StateMaster))
		hdr := ptp.Header{MessageType: ptp.MsgDelayReq, TransportSpecific: 0, VersionPTP: ptp.PTPVersion2, DomainNumber: 254, SourcePortIdentity: slaveID}
		svc.handleEventPacket(ptpport.Packet{Data: ptp.EncodeDelayReq(hdr, ptp.DelayReqBody{}), Timestamp: time.Now()})
		if len(tr.sentGenerals()) != 0 {
			t.Fatal("P2P profile answered DelayReq")
		}
	})
}

func TestPowerProfileRejectsGPTPTransportSpecific(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(254)
	cfg.DomainNumber = &domainNumber
	svc := New(tr, testSelfID(), cfg, testLogger())
	svc.state.Store(uint32(StateMaster))

	hdr := ptp.Header{
		MessageType:       ptp.MsgPDelayReq,
		TransportSpecific: 1, // IEEE 802.1AS/gPTP, not C37.238
		VersionPTP:        ptp.PTPVersion2,
		DomainNumber:      254,
		SourcePortIdentity: ptp.PortIdentity{
			ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{1, 2, 3, 4, 5, 6}),
			PortNumber:    1,
		},
	}
	svc.handleEventPacket(ptpport.Packet{
		Data:      ptp.EncodePDelayReq(hdr, ptp.PDelayReqBody{}),
		Timestamp: time.Now(),
	})
	if len(tr.sentEvents()) != 0 || len(tr.sentGenerals()) != 0 {
		t.Fatal("C37.238 server answered an IEEE 802.1AS message")
	}
}

func TestPriorityZeroOverride(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	zero := uint8(0)
	cfg.Priority1 = &zero
	cfg.Priority2 = &zero
	svc := New(tr, testSelfID(), cfg, testLogger())
	snapshot := svc.Snapshot()
	if snapshot.Priority1 != 0 || snapshot.Priority2 != 0 {
		t.Fatalf("zero priorities were not applied: %+v", snapshot)
	}
}

func TestRunTransitionsToFaultyOnTransportError(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	want := errors.New("receive failed")
	go func() { tr.errCh <- want }()
	err := svc.Run(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Run error: want %v, got %v", want, err)
	}
	if svc.State() != StateFaulty {
		t.Fatalf("state: want FAULTY, got %s", svc.State())
	}
}

func TestSendAnnounceC372382011(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(0)
	cfg.DomainNumber = &domainNumber
	cfg.C37238Version = ptp.C37238Version2011
	cfg.C37238GrandmasterID = 3
	cfg.GrandmasterTimeInaccuracy = 60
	cfg.NetworkTimeInaccuracy = 0
	cfg.TotalTimeInaccuracy = 100
	cfg.AlternateTimeOffset = &ptp.AlternateTimeOffsetTLV{
		KeyField: 1, CurrentOffset: 10763, DisplayName: "UTC+03:00",
	}
	svc := New(tr, testSelfID(), cfg, testLogger())

	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}

	generals := tr.sentGenerals()
	if len(generals) != 1 {
		t.Fatalf("expected 1 general message, got %d", len(generals))
	}

	hdr, err := ptp.DecodeHeader(generals[0])
	if err != nil {
		t.Fatal(err)
	}
	if hdr.TransportSpecific != 0 {
		t.Errorf("TransportSpecific: want 0 (C37.238), got %d", hdr.TransportSpecific)
	}
	if hdr.DomainNumber != 0 {
		t.Errorf("DomainNumber: want 0, got %d", hdr.DomainNumber)
	}
	if hdr.LogMessageInterval != 1 {
		t.Errorf("logMessageInterval: want 1, got %d", hdr.LogMessageInterval)
	}

	tlvOff := ptp.HeaderSize + 30
	data := generals[0]
	if len(data) != tlvOff+22+30 {
		t.Fatalf("C37.238 Announce length: want %d, got %d", tlvOff+22+30, len(data))
	}
	if got := binary.BigEndian.Uint16(data[tlvOff:]); got != ptp.TLVOrganizationExtension {
		t.Fatalf("TLV: want ORGANIZATION_EXTENSION, got 0x%04X", got)
	}
	if data[tlvOff+4] != 0x1C || data[tlvOff+5] != 0x12 || data[tlvOff+6] != 0x9D {
		t.Errorf("C37.238 OUI: want 1C:12:9D, got %02X:%02X:%02X",
			data[tlvOff+4], data[tlvOff+5], data[tlvOff+6])
	}
	if got := [3]byte(data[tlvOff+7 : tlvOff+10]); got != ptp.C37238OrgSubType2011 {
		t.Fatalf("C37.238 subtype: want % X, got % X", ptp.C37238OrgSubType2011, got)
	}
	if got := binary.BigEndian.Uint16(data[tlvOff+10:]); got != 3 {
		t.Fatalf("Grandmaster ID: want 3, got %d", got)
	}
	if got := binary.BigEndian.Uint32(data[tlvOff+12:]); got != 60 {
		t.Fatalf("grandmasterTimeInaccuracy: want 60, got %d", got)
	}
	if got := binary.BigEndian.Uint16(data[tlvOff+22:]); got != ptp.TLVAlternateTimeOffset {
		t.Fatalf("next TLV: want ALTERNATE_TIME_OFFSET_INDICATOR, got 0x%04X", got)
	}

	snapshot := svc.Snapshot()
	if snapshot.TransportSpecific != 0 || snapshot.C37238Version != ptp.C37238Version2011 {
		t.Fatalf("Power Profile snapshot is incomplete: %+v", snapshot)
	}
	if snapshot.LogSyncInterval != 0 || snapshot.LogAnnounceInterval != 1 {
		t.Fatalf("Power Profile intervals are wrong: %+v", snapshot)
	}
	if snapshot.C37238GrandmasterID != 3 || snapshot.GrandmasterTimeInaccuracy != 60 || snapshot.NetworkTimeInaccuracy != 0 || snapshot.AlternateTimeOffset == nil {
		t.Fatalf("Power Profile TLV values are wrong: %+v", snapshot)
	}
}

func TestSendAnnounceC372382017(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(254)
	cfg.DomainNumber = &domainNumber
	cfg.C37238Version = ptp.C37238Version2017
	cfg.C37238GrandmasterID = 3
	cfg.TotalTimeInaccuracy = 100
	cfg.AlternateTimeOffset = &ptp.AlternateTimeOffsetTLV{
		KeyField: 1, CurrentOffset: 10763, DisplayName: "UTC+03:00",
	}
	svc := New(tr, testSelfID(), cfg, testLogger())

	if err := svc.sendAnnounce(); err != nil {
		t.Fatal(err)
	}
	data := tr.sentGenerals()[0]
	tlvOff := ptp.HeaderSize + 30
	if len(data) != tlvOff+22+30 {
		t.Fatalf("C37.238-2017 Announce length: want %d, got %d", tlvOff+22+30, len(data))
	}
	if got := [3]byte(data[tlvOff+7 : tlvOff+10]); got != ptp.C37238OrgSubType2017 {
		t.Fatalf("C37.238 subtype: want % X, got % X", ptp.C37238OrgSubType2017, got)
	}
	if got := binary.BigEndian.Uint32(data[tlvOff+16:]); got != 100 {
		t.Fatalf("totalTimeInaccuracy: want 100, got %d", got)
	}
	if got := binary.BigEndian.Uint16(data[tlvOff+22:]); got != ptp.TLVAlternateTimeOffset {
		t.Fatalf("next TLV: want ALTERNATE_TIME_OFFSET_INDICATOR, got 0x%04X", got)
	}
}

func TestSendAnnounceSequence(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	for i := 0; i < 3; i++ {
		if err := svc.sendAnnounce(); err != nil {
			t.Fatal(err)
		}
	}

	generals := tr.sentGenerals()
	for i, data := range generals {
		hdr, _ := ptp.DecodeHeader(data)
		if hdr.SequenceID != uint16(i) {
			t.Errorf("announce #%d: SequenceID want %d, got %d", i, i, hdr.SequenceID)
		}
	}
}

func TestSendSync(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	if err := svc.sendSync(); err != nil {
		t.Fatal(err)
	}

	events := tr.sentEvents()
	generals := tr.sentGenerals()
	if len(events) != 1 {
		t.Fatalf("expected 1 event (SYNC), got %d", len(events))
	}
	if len(generals) != 1 {
		t.Fatalf("expected 1 general (FOLLOW_UP), got %d", len(generals))
	}

	// Verify SYNC
	syncHdr, err := ptp.DecodeHeader(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if syncHdr.MessageType != ptp.MsgSync {
		t.Errorf("SYNC MessageType: want %d, got %d", ptp.MsgSync, syncHdr.MessageType)
	}
	if syncHdr.FlagField&ptp.FlagTwoStep == 0 {
		t.Error("SYNC should have TWO_STEP flag")
	}
	if syncHdr.SequenceID != 0 {
		t.Errorf("SYNC SequenceID: want 0, got %d", syncHdr.SequenceID)
	}

	// Verify FOLLOW_UP
	fuHdr, err := ptp.DecodeHeader(generals[0])
	if err != nil {
		t.Fatal(err)
	}
	if fuHdr.MessageType != ptp.MsgFollowUp {
		t.Errorf("FOLLOW_UP MessageType: want %d, got %d", ptp.MsgFollowUp, fuHdr.MessageType)
	}
	if fuHdr.SequenceID != syncHdr.SequenceID {
		t.Errorf("FOLLOW_UP SequenceID should match SYNC: want %d, got %d", syncHdr.SequenceID, fuHdr.SequenceID)
	}
	if fuHdr.FlagField != 0 {
		t.Errorf("FOLLOW_UP FlagField: want 0, got 0x%04X", fuHdr.FlagField)
	}

	// Verify FOLLOW_UP carries the exact TAI timestamp.
	fuBody, err := ptp.DecodeFollowUpBody(generals[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if want := ptp.TimestampFromTimeWithUTCOffset(tr.txTime, 37); fuBody.PreciseOriginTimestamp != want {
		t.Errorf("FOLLOW_UP t1: want %+v, got %+v", want, fuBody.PreciseOriginTimestamp)
	}
}

func TestSendSyncRejectsEmptyTransportTimestamp(t *testing.T) {
	tr := newMockTransport()
	tr.txTime = time.Time{}
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	if err := svc.sendSync(); err == nil {
		t.Fatal("expected empty TX timestamp error")
	}
	if generals := tr.sentGenerals(); len(generals) != 0 {
		t.Fatalf("FollowUp was sent with an empty transport timestamp: %d packets", len(generals))
	}
}

func TestSendSyncSequence(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	for i := 0; i < 3; i++ {
		if err := svc.sendSync(); err != nil {
			t.Fatal(err)
		}
	}

	events := tr.sentEvents()
	for i, data := range events {
		hdr, _ := ptp.DecodeHeader(data)
		if hdr.SequenceID != uint16(i) {
			t.Errorf("sync #%d: SequenceID want %d, got %d", i, i, hdr.SequenceID)
		}
	}
}

func TestHandleDelayReq(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	svc.state.Store(uint32(StateMaster))

	// Build a DELAY_REQ from a slave
	slaveID := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}),
		PortNumber:    1,
	}
	reqHdr := ptp.Header{
		MessageType:        ptp.MsgDelayReq,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       0,
		CorrectionField:    999 << 16,
		SourcePortIdentity: slaveID,
		SequenceID:         42,
		ControlField:       ptp.ControlDelayReq,
	}
	reqBody := ptp.DelayReqBody{OriginTimestamp: ptp.TimestampFromTime(time.Now())}
	reqData := ptp.EncodeDelayReq(reqHdr, reqBody)

	rxTime := time.Date(2025, 6, 1, 12, 0, 1, 0, time.UTC)
	pkt := ptpport.Packet{
		Data:      reqData,
		Timestamp: rxTime,
	}

	svc.handleEventPacket(pkt)

	generals := tr.sentGenerals()
	if len(generals) != 1 {
		t.Fatalf("expected 1 DELAY_RESP, got %d", len(generals))
	}

	respHdr, err := ptp.DecodeHeader(generals[0])
	if err != nil {
		t.Fatal(err)
	}
	if respHdr.MessageType != ptp.MsgDelayResp {
		t.Errorf("MessageType: want DELAY_RESP, got %d", respHdr.MessageType)
	}
	if respHdr.SequenceID != 42 {
		t.Errorf("SequenceID: want 42, got %d", respHdr.SequenceID)
	}
	if respHdr.CorrectionField != 999<<16 {
		t.Errorf("CorrectionField: want %d, got %d (should propagate from request)", 999<<16, respHdr.CorrectionField)
	}
	if respHdr.DomainNumber != 0 {
		t.Errorf("DomainNumber: want 0 (echo from request), got %d", respHdr.DomainNumber)
	}

	respBody, err := ptp.DecodeDelayRespBody(generals[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if respBody.RequestingPortIdentity != slaveID {
		t.Errorf("RequestingPortIdentity: want %v, got %v", slaveID, respBody.RequestingPortIdentity)
	}
	if want := ptp.TimestampFromTimeWithUTCOffset(rxTime, 37); respBody.ReceiveTimestamp != want {
		t.Errorf("DELAY_RESP t4: want %+v, got %+v", want, respBody.ReceiveTimestamp)
	}
}

func TestDelayReqFromSameClockIdentityDifferentPortIsAccepted(t *testing.T) {
	tr := newMockTransport()
	self := testSelfID()
	svc := New(tr, self, testConfig(), testLogger())
	svc.state.Store(uint32(StateMaster))
	requester := self
	requester.PortNumber = self.PortNumber + 1
	hdr := ptp.Header{
		MessageType:        ptp.MsgDelayReq,
		VersionPTP:         ptp.PTPVersion2,
		SourcePortIdentity: requester,
		ControlField:       ptp.ControlDelayReq,
	}
	svc.handleEventPacket(ptpport.Packet{
		Data:      ptp.EncodeDelayReq(hdr, ptp.DelayReqBody{}),
		Timestamp: time.Now(),
	})
	if got := len(tr.sentGenerals()); got != 1 {
		t.Fatalf("DelayReq from another local PortIdentity: want one response, got %d", got)
	}
}

func TestHandlePDelayReq(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(254)
	cfg.DomainNumber = &domainNumber
	svc := New(tr, testSelfID(), cfg, testLogger())
	svc.state.Store(uint32(StateMaster))

	slaveID := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}),
		PortNumber:    1,
	}
	reqHdr := ptp.Header{
		MessageType:        ptp.MsgPDelayReq,
		TransportSpecific:  0,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       254,
		CorrectionField:    555 << 16,
		SourcePortIdentity: slaveID,
		SequenceID:         77,
		ControlField:       ptp.ControlOther,
	}
	reqData := ptp.EncodePDelayReq(reqHdr, ptp.PDelayReqBody{})

	rxTime := time.Date(2025, 6, 1, 12, 0, 0, 100000000, time.UTC)
	pkt := ptpport.Packet{
		Data:      reqData,
		Timestamp: rxTime,
	}

	svc.handleEventPacket(pkt)

	events := tr.sentEvents()
	generals := tr.sentGenerals()
	if len(events) != 1 {
		t.Fatalf("expected 1 PDELAY_RESP event, got %d", len(events))
	}
	if len(generals) != 1 {
		t.Fatalf("expected 1 PDELAY_RESP_FOLLOW_UP general, got %d", len(generals))
	}
	if eventTo, generalTo := tr.unicastCounts(); eventTo != 0 || generalTo != 0 {
		t.Fatalf("multicast PDelay request produced unicast replies: event=%d general=%d", eventTo, generalTo)
	}

	// PDELAY_RESP
	respHdr, err := ptp.DecodeHeader(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if respHdr.MessageType != ptp.MsgPDelayResp {
		t.Errorf("MessageType: want PDELAY_RESP, got %d", respHdr.MessageType)
	}
	if respHdr.FlagField&ptp.FlagTwoStep == 0 {
		t.Error("PDELAY_RESP should have TWO_STEP flag")
	}
	if respHdr.SequenceID != 77 {
		t.Errorf("SequenceID: want 77, got %d", respHdr.SequenceID)
	}
	// PDELAY_RESP transportSpecific comes from the request.
	if respHdr.TransportSpecific != 0 {
		t.Errorf("PDELAY_RESP TransportSpecific: want 0 (echo), got %d", respHdr.TransportSpecific)
	}
	// PDELAY_RESP domainNumber comes from the request.
	if respHdr.DomainNumber != 254 {
		t.Errorf("PDELAY_RESP DomainNumber: want 254 (echo), got %d", respHdr.DomainNumber)
	}

	respBody, err := ptp.DecodePDelayRespBody(events[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if respBody.RequestingPortIdentity != slaveID {
		t.Errorf("RequestingPortIdentity: want %v, got %v", slaveID, respBody.RequestingPortIdentity)
	}
	if want := ptp.TimestampFromTimeWithUTCOffset(rxTime, 37); respBody.RequestReceiptTimestamp != want {
		t.Errorf("PDELAY_RESP t2: want %+v, got %+v", want, respBody.RequestReceiptTimestamp)
	}

	// PDELAY_RESP_FOLLOW_UP
	fuHdr, err := ptp.DecodeHeader(generals[0])
	if err != nil {
		t.Fatal(err)
	}
	if fuHdr.MessageType != ptp.MsgPDelayRespFollowUp {
		t.Errorf("MessageType: want PDELAY_RESP_FOLLOW_UP, got %d", fuHdr.MessageType)
	}
	if fuHdr.CorrectionField != 555<<16 {
		t.Errorf("CorrectionField: want %d, got %d (should propagate from request)", 555<<16, fuHdr.CorrectionField)
	}
	if fuHdr.SequenceID != 77 {
		t.Errorf("SequenceID: want 77, got %d", fuHdr.SequenceID)
	}
	// FOLLOW_UP domainNumber comes from the request.
	if fuHdr.DomainNumber != 254 {
		t.Errorf("PDELAY_RESP_FOLLOW_UP DomainNumber: want 254 (echo), got %d", fuHdr.DomainNumber)
	}
	// FOLLOW_UP uses the local transportSpecific (from the profile, 0 for default).
	if fuHdr.TransportSpecific != 0 {
		t.Errorf("PDELAY_RESP_FOLLOW_UP TransportSpecific: want 0 (own), got %d", fuHdr.TransportSpecific)
	}
	fuBody, err := ptp.DecodePDelayRespFollowUpBody(generals[0][ptp.HeaderSize:])
	if err != nil {
		t.Fatal(err)
	}
	if want := ptp.TimestampFromTimeWithUTCOffset(tr.txTime, 37); fuBody.ResponseOriginTimestamp != want {
		t.Errorf("PDELAY_RESP_FOLLOW_UP t3: want %+v, got %+v", want, fuBody.ResponseOriginTimestamp)
	}
	if got := svc.Snapshot().PDelayRespCount; got != 1 {
		t.Errorf("PDelay response count: want 1, got %d", got)
	}
}

func TestDelayReqIgnoredWhenNotMaster(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	// State is INITIALIZING (not MASTER) — should not respond
	svc.state.Store(uint32(StateInitializing))

	slaveID := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}),
		PortNumber:    1,
	}
	reqHdr := ptp.Header{
		MessageType:        ptp.MsgDelayReq,
		VersionPTP:         ptp.PTPVersion2,
		SourcePortIdentity: slaveID,
		SequenceID:         1,
		ControlField:       ptp.ControlDelayReq,
	}
	reqData := ptp.EncodeDelayReq(reqHdr, ptp.DelayReqBody{})
	pkt := ptpport.Packet{Data: reqData, Timestamp: time.Now()}

	svc.handleEventPacket(pkt)

	if len(tr.sentGenerals()) != 0 {
		t.Error("should not respond to DELAY_REQ when not in MASTER state")
	}
}

func TestIgnoresOwnMessages(t *testing.T) {
	tr := newMockTransport()
	selfID := testSelfID()
	svc := New(tr, selfID, testConfig(), testLogger())
	svc.state.Store(uint32(StateMaster))

	// Build a DELAY_REQ from ourselves
	reqHdr := ptp.Header{
		MessageType:        ptp.MsgDelayReq,
		VersionPTP:         ptp.PTPVersion2,
		SourcePortIdentity: selfID,
		SequenceID:         1,
		ControlField:       ptp.ControlDelayReq,
	}
	reqData := ptp.EncodeDelayReq(reqHdr, ptp.DelayReqBody{})
	pkt := ptpport.Packet{Data: reqData, Timestamp: time.Now()}

	svc.handleEventPacket(pkt)

	if len(tr.sentGenerals()) != 0 {
		t.Error("should ignore own messages")
	}
}

func TestIgnoresForeignDomainMessages(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())
	svc.state.Store(uint32(StateMaster))

	reqHdr := ptp.Header{
		MessageType:  ptp.MsgDelayReq,
		VersionPTP:   ptp.PTPVersion2,
		DomainNumber: 44,
		SourcePortIdentity: ptp.PortIdentity{
			ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}),
			PortNumber:    1,
		},
		SequenceID:   1,
		ControlField: ptp.ControlDelayReq,
	}
	reqData := ptp.EncodeDelayReq(reqHdr, ptp.DelayReqBody{})
	pkt := ptpport.Packet{Data: reqData, Timestamp: time.Now()}

	svc.handleEventPacket(pkt)

	if len(tr.sentGenerals()) != 0 {
		t.Error("should ignore DELAY_REQ from a foreign domain")
	}
}

func TestDomainZeroOverride(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.Profile = ptp.PowerProfile
	domainNumber := uint8(0)
	cfg.DomainNumber = &domainNumber
	svc := New(tr, testSelfID(), cfg, testLogger())

	if svc.profile.DomainNumber != 0 {
		t.Fatalf("DomainNumber: want 0 override, got %d", svc.profile.DomainNumber)
	}
}

func TestRunStartsAndStops(t *testing.T) {
	tr := newMockTransport()
	svc := New(tr, testSelfID(), testConfig(), testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := svc.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if svc.State() != StateStopped {
		t.Errorf("state after Run: want STOPPED, got %s", svc.State())
	}
}

func TestTAIConversion(t *testing.T) {
	tr := newMockTransport()
	cfg := testConfig()
	cfg.UTCOffset = 37
	svc := New(tr, testSelfID(), cfg, testLogger())

	utcTime := time.Date(2025, 6, 1, 12, 0, 0, 500000000, time.UTC)
	tai := svc.tai(utcTime)

	expectedSec := uint64(utcTime.Unix()) + 37
	if tai.Seconds != expectedSec {
		t.Errorf("TAI seconds: want %d, got %d", expectedSec, tai.Seconds)
	}
	if tai.Nanoseconds != 500000000 {
		t.Errorf("TAI nanoseconds: want 500000000, got %d", tai.Nanoseconds)
	}
}
