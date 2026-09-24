// Package ptpserver implements a PTPv2 grandmaster clock (IEEE 1588-2008).
// Designated master FSM: INITIALIZING → MASTER → FAULTY.
// It runs three concurrent loops: announceLoop, syncLoop, and receiveLoop.
// It responds to E2E (DELAY_REQ) or P2P (PDELAY_REQ) according to the selected profile.
package ptpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"pbmt/internal/application/ptpport"
	"pbmt/internal/domain/ptp"
)

// PortState is the port state in the designated-master FSM.
type PortState uint8

const (
	StateInitializing PortState = iota
	StateMaster
	StateFaulty
	StateStopped
)

func (s PortState) String() string {
	switch s {
	case StateInitializing:
		return "INITIALIZING"
	case StateMaster:
		return "MASTER"
	case StateFaulty:
		return "FAULTY"
	case StateStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// Config configures the PTP server.
type Config struct {
	Profile                   ptp.Profile
	Transport                 string // "udp" or "ethernet"
	DomainNumber              *uint8
	DelayMechanism            uint8 // 0 uses the profile; otherwise DelayMechanismE2E or DelayMechanismP2P
	UTCOffset                 int16 // TAI-UTC offset (currently about 37 s)
	TimeSource                uint8 // timeSource for ANNOUNCE
	Priority1                 *uint8
	Priority2                 *uint8
	ClockClass                *uint8
	ClockAccuracy             *uint8
	ClockVariance             *uint16
	TimeTraceable             *bool
	FrequencyTraceable        *bool
	C37238Version             ptp.C37238Version
	C37238GrandmasterID       uint16
	GrandmasterTimeInaccuracy uint32
	NetworkTimeInaccuracy     uint32
	TotalTimeInaccuracy       uint32
	AlternateTimeOffset       *ptp.AlternateTimeOffsetTLV
}

// Snapshot contains the current Grandmaster parameters and counters for the GUI.
type Snapshot struct {
	State                     PortState
	Profile                   string
	DelayMechanism            string
	TwoStep                   bool
	DomainNumber              uint8
	CurrentUTCOffset          int16
	Priority1                 uint8
	Priority2                 uint8
	GrandmasterClockClass     uint8
	GrandmasterClockAccuracy  uint8
	GrandmasterClockVariance  uint16
	GrandmasterClockIdentity  ptp.ClockIdentity
	PortNumber                uint16
	TimeSource                uint8
	TransportSpecific         uint8
	LogSyncInterval           int8
	LogAnnounceInterval       int8
	C37238Version             ptp.C37238Version
	C37238GrandmasterID       uint16
	GrandmasterTimeInaccuracy uint32
	NetworkTimeInaccuracy     uint32
	TotalTimeInaccuracy       uint32
	AlternateTimeOffset       *ptp.AlternateTimeOffsetTLV
	UTCOffsetValid            bool
	TimeTraceable             bool
	FrequencyTraceable        bool
	Transport                 string
	AnnounceCount             uint64
	SyncCount                 uint64
	DelayRespCount            uint64
	PDelayRespCount           uint64
}

// Service is a PTP server implemented as a grandmaster-only ordinary clock.
type Service struct {
	cfg     Config
	profile ptp.Profile
	tr      ptpport.Transport
	log     *slog.Logger

	selfID ptp.PortIdentity
	state  atomic.Uint32 // PortState

	// Independent sequence counters for each message type.
	announceSeq atomic.Uint32
	syncSeq     atomic.Uint32
	announceTx  atomic.Uint64
	syncTx      atomic.Uint64
	delayRespTx atomic.Uint64
	pdelayTx    atomic.Uint64
}

// New creates a PTP server.
func New(
	tr ptpport.Transport,
	selfID ptp.PortIdentity,
	cfg Config,
	log *slog.Logger,
) *Service {
	profile := cfg.Profile
	if cfg.DomainNumber != nil {
		profile.DomainNumber = *cfg.DomainNumber
	}
	if cfg.C37238Version != ptp.C37238Disabled {
		profile.C37238Version = cfg.C37238Version
	}
	if profile.C37238Version != ptp.C37238Disabled {
		profile.DelayMechanism = ptp.DelayMechanismP2P
	} else if cfg.DelayMechanism != 0 {
		profile.DelayMechanism = cfg.DelayMechanism
	}
	if cfg.Priority1 != nil {
		profile.Priority1 = *cfg.Priority1
	}
	if cfg.Priority2 != nil {
		profile.Priority2 = *cfg.Priority2
	}
	if cfg.ClockClass != nil {
		profile.ClockClass = *cfg.ClockClass
	}
	if cfg.ClockAccuracy != nil {
		profile.ClockAccuracy = *cfg.ClockAccuracy
	}
	if cfg.ClockVariance != nil {
		profile.ClockVariance = *cfg.ClockVariance
	}
	if cfg.TimeSource != 0 {
		profile.TimeSource = cfg.TimeSource
	}
	applyFlagOverride(&profile.FlagField, ptp.FlagTimeTraceable, cfg.TimeTraceable)
	applyFlagOverride(&profile.FlagField, ptp.FlagFrequencyTraceable, cfg.FrequencyTraceable)

	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		cfg:     cfg,
		profile: profile,
		tr:      tr,
		log:     log,
		selfID:  selfID,
	}
	s.state.Store(uint32(StateInitializing))
	return s
}

func applyFlagOverride(flags *uint16, mask uint16, enabled *bool) {
	if enabled == nil {
		return
	}
	if *enabled {
		*flags |= mask
	} else {
		*flags &^= mask
	}
}

// Run starts the PTP server and blocks until the context is canceled.
func (s *Service) Run(ctx context.Context) error {
	if mode := s.tr.TimestampMode(); mode != "hardware" {
		s.state.Store(uint32(StateFaulty))
		return fmt.Errorf("ptp_server requires hardware timestamping, got %q", mode)
	}
	if _, ok := s.tr.(ptpport.TimeSource); !ok {
		s.state.Store(uint32(StateFaulty))
		return errors.New("ptp_server hardware transport does not provide its clock")
	}
	s.tr.Start()
	defer s.tr.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.state.Store(uint32(StateMaster))
	s.log.Debug("ptp_server started",
		"state", "MASTER",
		"profile", s.profile.Name,
		"domain", s.profile.DomainNumber,
		"transport", s.cfg.Transport,
		"timestamp_mode", "hardware",
		"self_id", s.selfID.String(),
		"utc_offset", s.cfg.UTCOffset,
		"clock_class", s.profile.ClockClass,
		"clock_accuracy", s.profile.ClockAccuracy,
		"priority1", s.profile.Priority1,
		"priority2", s.profile.Priority2,
		"time_source", s.profile.TimeSource,
		"sync_interval", s.profile.SyncInterval(),
		"announce_interval", s.profile.AnnounceInterval(),
	)

	var wg sync.WaitGroup
	results := make(chan error, 3)
	wg.Add(3)

	go func() {
		defer wg.Done()
		results <- s.announceLoop(runCtx)
	}()
	go func() {
		defer wg.Done()
		results <- s.syncLoop(runCtx)
	}()
	go func() {
		defer wg.Done()
		results <- s.receiveLoop(runCtx)
	}()

	err := <-results
	cancel()
	wg.Wait()
	if err != nil {
		s.state.Store(uint32(StateFaulty))
		s.log.Debug("ptp_server stopped with error", "err", err)
		return err
	}
	s.state.Store(uint32(StateStopped))
	s.log.Debug("ptp_server stopped")
	return nil
}

// State returns the current port state.
func (s *Service) State() PortState { return PortState(s.state.Load()) }

// Snapshot returns a consistent snapshot of the effective server configuration.
func (s *Service) Snapshot() Snapshot {
	return Snapshot{
		State:                     s.State(),
		Profile:                   s.profile.Name,
		DelayMechanism:            ptp.DelayMechanismName(s.profile.DelayMechanism),
		TwoStep:                   true,
		DomainNumber:              s.profile.DomainNumber,
		CurrentUTCOffset:          s.cfg.UTCOffset,
		Priority1:                 s.profile.Priority1,
		Priority2:                 s.profile.Priority2,
		GrandmasterClockClass:     s.profile.ClockClass,
		GrandmasterClockAccuracy:  s.profile.ClockAccuracy,
		GrandmasterClockVariance:  s.profile.ClockVariance,
		GrandmasterClockIdentity:  s.selfID.ClockIdentity,
		PortNumber:                s.selfID.PortNumber,
		TimeSource:                s.profile.TimeSource,
		TransportSpecific:         s.profile.TransportSpecific,
		LogSyncInterval:           s.profile.LogSyncInterval,
		LogAnnounceInterval:       s.profile.LogAnnounceInterval,
		C37238Version:             s.profile.C37238Version,
		C37238GrandmasterID:       s.cfg.C37238GrandmasterID,
		GrandmasterTimeInaccuracy: s.cfg.GrandmasterTimeInaccuracy,
		NetworkTimeInaccuracy:     s.cfg.NetworkTimeInaccuracy,
		TotalTimeInaccuracy:       s.cfg.TotalTimeInaccuracy,
		AlternateTimeOffset:       cloneAlternateTimeOffset(s.cfg.AlternateTimeOffset),
		UTCOffsetValid:            s.profile.FlagField&ptp.FlagCurrentUtcOffsetValid != 0,
		TimeTraceable:             s.profile.FlagField&ptp.FlagTimeTraceable != 0,
		FrequencyTraceable:        s.profile.FlagField&ptp.FlagFrequencyTraceable != 0,
		Transport:                 s.cfg.Transport,
		AnnounceCount:             s.announceTx.Load(),
		SyncCount:                 s.syncTx.Load(),
		DelayRespCount:            s.delayRespTx.Load(),
		PDelayRespCount:           s.pdelayTx.Load(),
	}
}

// tai converts a UTC time.Time to a TAI PTP Timestamp.
// IEEE 1588-2008 Section 7.2.3: PTP_TIMESCALE timestamps use TAI.
func (s *Service) tai(t time.Time) ptp.Timestamp {
	return ptp.TimestampFromTimeWithUTCOffset(t, s.cfg.UTCOffset)
}

func (s *Service) now() (time.Time, error) {
	if source, ok := s.tr.(ptpport.TimeSource); ok {
		return source.Now()
	}
	return time.Time{}, errors.New("ptp_server hardware transport does not provide its clock")
}

// ── announceLoop ─────────────────────────────────────────────────────────

func (s *Service) announceLoop(ctx context.Context) error {
	if err := s.sendAnnounce(); err != nil {
		return fmt.Errorf("send initial Announce: %w", err)
	}
	interval := time.Duration(s.profile.AnnounceInterval() * float64(time.Second))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.sendAnnounce(); err != nil {
				return fmt.Errorf("send Announce: %w", err)
			}
		}
	}
}

func (s *Service) sendAnnounce() error {
	clockNow, err := s.now()
	if err != nil {
		return fmt.Errorf("read Grandmaster clock: %w", err)
	}
	now := s.tai(clockNow)
	seq := uint16(s.announceSeq.Add(1) - 1)

	h := ptp.Header{
		MessageType:        ptp.MsgAnnounce,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       s.profile.DomainNumber,
		FlagField:          s.profile.FlagField,
		SourcePortIdentity: s.selfID,
		SequenceID:         seq,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: s.profile.LogAnnounceInterval,
	}

	body := ptp.AnnounceBody{
		OriginTimestamp:      now,
		CurrentUtcOffset:     s.cfg.UTCOffset,
		GrandmasterPriority1: s.profile.Priority1,
		GrandmasterClockQuality: ptp.ClockQuality{
			ClockClass:              s.profile.ClockClass,
			ClockAccuracy:           s.profile.ClockAccuracy,
			OffsetScaledLogVariance: s.profile.ClockVariance,
		},
		GrandmasterPriority2: s.profile.Priority2,
		GrandmasterIdentity:  s.selfID.ClockIdentity,
		StepsRemoved:         0,
		TimeSource:           s.profile.TimeSource,
	}

	var data []byte
	switch s.profile.C37238Version {
	case ptp.C37238Version2011:
		tlv := ptp.C37238TLV2011{
			GrandmasterID:             s.cfg.C37238GrandmasterID,
			GrandmasterTimeInaccuracy: s.cfg.GrandmasterTimeInaccuracy,
			NetworkTimeInaccuracy:     s.cfg.NetworkTimeInaccuracy,
		}
		data = ptp.EncodeAnnounceC372382011(h, body, tlv, s.cfg.AlternateTimeOffset)
	case ptp.C37238Version2017:
		tlv := ptp.C37238TLV2017{
			GrandmasterID:       s.cfg.C37238GrandmasterID,
			TotalTimeInaccuracy: s.cfg.TotalTimeInaccuracy,
		}
		data = ptp.EncodeAnnounceC372382017(h, body, tlv, s.cfg.AlternateTimeOffset)
	default:
		data = ptp.EncodeAnnounce(h, body)
	}

	if err := s.tr.SendGeneral(data); err != nil {
		return err
	}
	s.announceTx.Add(1)
	s.log.Debug("ptp_server: TX ANNOUNCE", "seq", seq)
	return nil
}

func cloneAlternateTimeOffset(value *ptp.AlternateTimeOffsetTLV) *ptp.AlternateTimeOffsetTLV {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// ── syncLoop ─────────────────────────────────────────────────────────────

func (s *Service) syncLoop(ctx context.Context) error {
	if err := s.sendSync(); err != nil {
		return fmt.Errorf("send initial Sync: %w", err)
	}
	interval := time.Duration(s.profile.SyncInterval() * float64(time.Second))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.sendSync(); err != nil {
				return fmt.Errorf("send Sync: %w", err)
			}
		}
	}
}

func (s *Service) sendSync() error {
	seq := uint16(s.syncSeq.Add(1) - 1)

	// Two-step: SYNC with a zero timestamp and FlagTwoStep.
	syncH := ptp.Header{
		MessageType:        ptp.MsgSync,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       s.profile.DomainNumber,
		FlagField:          ptp.FlagTwoStep,
		SourcePortIdentity: s.selfID,
		SequenceID:         seq,
		ControlField:       ptp.ControlSync,
		LogMessageInterval: s.profile.LogSyncInterval,
	}
	syncData := ptp.EncodeSync(syncH, ptp.SyncBody{})

	txTime, err := s.tr.SendEvent(syncData)
	if err != nil {
		return err
	}
	if txTime.IsZero() {
		return errors.New("PTP transport returned an empty Sync TX timestamp")
	}

	// FOLLOW_UP with the precise SYNC transmission timestamp.
	fuH := ptp.Header{
		MessageType:        ptp.MsgFollowUp,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       s.profile.DomainNumber,
		FlagField:          0,
		SourcePortIdentity: s.selfID,
		SequenceID:         seq,
		ControlField:       ptp.ControlFollowUp,
		LogMessageInterval: s.profile.LogSyncInterval,
	}
	fuData := ptp.EncodeFollowUp(fuH, ptp.FollowUpBody{
		PreciseOriginTimestamp: s.tai(txTime),
	})

	if err := s.tr.SendGeneral(fuData); err != nil {
		return err
	}
	s.syncTx.Add(1)
	return nil
}

// ── receiveLoop ──────────────────────────────────────────────────────────

func (s *Service) receiveLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt := <-s.tr.EventCh():
			s.handleEventPacket(pkt)
		case pkt := <-s.tr.GeneralCh():
			_ = pkt // the grandmaster does not process incoming general messages
		case err, ok := <-s.tr.Errors():
			if !ok {
				return errors.New("PTP transport stopped")
			}
			if err != nil {
				return err
			}
		}
	}
}

func (s *Service) handleEventPacket(pkt ptpport.Packet) {
	if pkt.Timestamp.IsZero() {
		s.log.Warn("ptp_server: ignored event without transport timestamp")
		return
	}
	if len(pkt.Data) < ptp.HeaderSize {
		return
	}
	hdr, err := ptp.DecodeHeader(pkt.Data)
	if err != nil {
		return
	}
	if hdr.VersionPTP != ptp.PTPVersion2 {
		return
	}
	if hdr.DomainNumber != s.profile.DomainNumber {
		return
	}
	if hdr.TransportSpecific != s.profile.TransportSpecific {
		return
	}
	// Ignore messages sent by this service.
	if hdr.SourcePortIdentity == s.selfID {
		return
	}

	switch hdr.MessageType {
	case ptp.MsgDelayReq:
		if hdr.MessageLength < ptp.HeaderSize+10 {
			return
		}
		s.handleDelayReq(hdr, pkt)
	case ptp.MsgPDelayReq:
		if hdr.MessageLength < ptp.HeaderSize+20 {
			return
		}
		s.handlePDelayReq(hdr, pkt)
	}
}

// handleDelayReq processes DELAY_REQ (E2E) and sends DELAY_RESP.
// IEEE 1588-2008 Section 11.3: CorrectionField is propagated from the request.
// Annex F.3.2: DELAY_RESP is sent to the primary multicast address by default.
// For hybrid/unicast E2E, the response is returned to the request's transport sender.
// Respond only in the MASTER state and echo domainNumber from the request.
func (s *Service) handleDelayReq(reqHdr ptp.Header, pkt ptpport.Packet) {
	if PortState(s.state.Load()) != StateMaster {
		return
	}
	if s.profile.DelayMechanism != ptp.DelayMechanismE2E {
		return
	}
	rxTime := pkt.Timestamp
	unicast := reqHdr.FlagField&ptp.FlagUnicast != 0
	flags := uint16(0)
	logMessageInterval := s.profile.LogDelayReqInterval
	if unicast {
		flags = ptp.FlagUnicast
		logMessageInterval = 0x7F
	}

	respH := ptp.Header{
		MessageType:        ptp.MsgDelayResp,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       reqHdr.DomainNumber, // echoed from the request
		FlagField:          flags,
		CorrectionField:    reqHdr.CorrectionField,
		SourcePortIdentity: s.selfID,
		SequenceID:         reqHdr.SequenceID,
		ControlField:       ptp.ControlDelayResp,
		LogMessageInterval: logMessageInterval,
	}
	respBody := ptp.DelayRespBody{
		ReceiveTimestamp:       s.tai(rxTime),
		RequestingPortIdentity: reqHdr.SourcePortIdentity,
	}
	data := ptp.EncodeDelayResp(respH, respBody)

	var err error
	if unicast {
		err = s.tr.SendGeneralTo(data, pkt.From)
	} else {
		err = s.tr.SendGeneral(data)
	}
	if err != nil {
		s.log.Warn("ptp_server: send DELAY_RESP", "err", err)
		return
	}
	s.delayRespTx.Add(1)
	s.log.Debug("ptp_server: TX DELAY_RESP",
		"seq", reqHdr.SequenceID,
		"from", reqHdr.SourcePortIdentity.String(),
	)
}

// handlePDelayReq processes PDELAY_REQ (P2P) and sends
// PDELAY_RESP (t2) + PDELAY_RESP_FOLLOW_UP (t3).
// In two-step mode, PDELAY_RESP carries requestReceiptTimestamp (t2),
// and PDELAY_RESP_FOLLOW_UP carries responseOriginTimestamp (t3).
// IEEE 1588-2008 Section 11.4.3: CorrectionField is propagated from the request to FOLLOW_UP.
// PDELAY_RESP takes transportSpecific from the request; PDELAY_RESP_FOLLOW_UP
// uses the local transportSpecific. Both messages echo domainNumber from the request.
func (s *Service) handlePDelayReq(reqHdr ptp.Header, pkt ptpport.Packet) {
	if PortState(s.state.Load()) != StateMaster {
		return
	}
	if s.profile.DelayMechanism != ptp.DelayMechanismP2P {
		return
	}
	rxTime := pkt.Timestamp // t2
	unicast := reqHdr.FlagField&ptp.FlagUnicast != 0
	flags := ptp.FlagTwoStep
	if unicast {
		flags |= ptp.FlagUnicast
	}

	respH := ptp.Header{
		MessageType:        ptp.MsgPDelayResp,
		TransportSpecific:  reqHdr.TransportSpecific, // echoed from the request
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       reqHdr.DomainNumber, // echoed from the request
		FlagField:          flags,
		SourcePortIdentity: s.selfID,
		SequenceID:         reqHdr.SequenceID,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	respBody := ptp.PDelayRespBody{
		RequestReceiptTimestamp: s.tai(rxTime),
		RequestingPortIdentity:  reqHdr.SourcePortIdentity,
	}
	respData := ptp.EncodePDelayResp(respH, respBody)

	var txTime time.Time
	var err error
	if unicast {
		txTime, err = s.tr.SendEventTo(respData, pkt.From)
	} else {
		txTime, err = s.tr.SendEvent(respData)
	}
	if err != nil {
		s.log.Warn("ptp_server: send PDELAY_RESP", "err", err)
		return
	}
	if txTime.IsZero() {
		s.log.Warn("ptp_server: send PDELAY_RESP", "err", "transport returned an empty TX timestamp")
		return
	}

	// PDELAY_RESP_FOLLOW_UP with the precise PDELAY_RESP transmission timestamp t3.
	fuH := ptp.Header{
		MessageType:        ptp.MsgPDelayRespFollowUp,
		TransportSpecific:  s.profile.TransportSpecific, // local value
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       reqHdr.DomainNumber, // echoed from the request
		FlagField:          flags & ptp.FlagUnicast,
		CorrectionField:    reqHdr.CorrectionField,
		SourcePortIdentity: s.selfID,
		SequenceID:         reqHdr.SequenceID,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	fuBody := ptp.PDelayRespFollowUpBody{
		ResponseOriginTimestamp: s.tai(txTime),
		RequestingPortIdentity:  reqHdr.SourcePortIdentity,
	}
	fuData := ptp.EncodePDelayRespFollowUp(fuH, fuBody)

	if unicast {
		err = s.tr.SendGeneralTo(fuData, pkt.From)
	} else {
		err = s.tr.SendGeneral(fuData)
	}
	if err != nil {
		s.log.Warn("ptp_server: send PDELAY_RESP_FOLLOW_UP", "err", err)
		return
	}
	s.pdelayTx.Add(1)
	s.log.Debug("ptp_server: TX PDELAY_RESP+FOLLOW_UP",
		"seq", reqHdr.SequenceID,
		"from", reqHdr.SourcePortIdentity.String(),
	)
}
