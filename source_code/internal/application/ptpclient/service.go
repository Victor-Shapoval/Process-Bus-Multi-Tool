package ptpclient

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"pbmt/internal/application/ptpport"
	"pbmt/internal/domain/clock"
	"pbmt/internal/domain/ptp"
)

// Config configures the PTP client.
type Config struct {
	Profile        ptp.Profile
	Transport      string // "udp" or "ethernet"
	TimestampMode  string // "software" or "hardware"
	DomainNumber   *uint8
	DelayMechanism uint8 // 0 uses the profile; otherwise DelayMechanismE2E or DelayMechanismP2P
}

// Snapshot contains the current PTP Client and application clock state.
type Snapshot struct {
	Source                    string
	State                     PortState
	Profile                   string
	DelayMechanism            string
	DomainNumber              uint8
	Transport                 string
	TimestampMode             string
	LocalPortIdentity         ptp.PortIdentity
	MasterPortIdentity        ptp.PortIdentity
	GrandmasterIdentity       ptp.ClockIdentity
	UTCOffset                 int16
	ExpectedC37238Version     ptp.C37238Version
	MasterC37238Version       ptp.C37238Version
	C37238GrandmasterID       uint16
	GrandmasterTimeInaccuracy uint32
	NetworkTimeInaccuracy     uint32
	TotalTimeInaccuracy       uint32
	AlternateTimeOffset       *ptp.AlternateTimeOffsetTLV
	PDelayRespCount           uint64
	TwoStep                   bool
	Offset                    time.Duration
	MeanPathDelay             time.Duration
	FrequencyPPB              float64
	ServoState                ServoState
	LastSync                  time.Time
	SyncCount                 uint64
	ApplicationTime           time.Time
	ClockStatus               clock.SourceStatus
}

const pendingFollowUpLimit = 8

type pendingFollowUp struct {
	header ptp.Header
	body   ptp.FollowUpBody
}

type pendingPDelayFollowUp struct {
	header ptp.Header
	body   ptp.PDelayRespFollowUpBody
}

// Service is a PTP client implemented as a slave-only ordinary clock.
type Service struct {
	cfg     Config
	profile ptp.Profile
	tr      ptpport.Transport
	clk     clock.Clock
	servo   *PIServo
	log     *slog.Logger

	selfID ptp.PortIdentity
	state  atomic.Uint32 // PortState
	tsproc TsProc

	// foreign master table
	mu             sync.Mutex
	foreignMasters map[ptp.PortIdentity]*ForeignMaster
	bestMaster     *ForeignMaster

	// sequence counters
	delayReqSeq      uint16
	pdelayReqSeq     uint16
	lastDelayReqSeq  uint16
	delayReqPending  bool
	lastPDelayReqSeq uint16
	pdelayReqPending bool
	pdelayRespSeq    uint16
	pdelayRespTx     atomic.Uint64

	// state tracking
	syncReceived bool
	lastSyncSeq  uint16
	twoStep      atomic.Bool
	utcOffset    int16

	// Event and general messages arrive through different transport channels.
	// Therefore, the receive loop may select FOLLOW_UP before its SYNC.
	pendingFollowUps []pendingFollowUp

	// pdelay state (P2P)
	pdelayT1             time.Time
	pdelayT2             time.Time
	pdelayT3             time.Time
	pdelayT4             time.Time
	pdelayRespCorrection int64
	pdelayRespReceived   bool
	pdelayRespSource     ptp.PortIdentity
	// PDELAY_RESP_FOLLOW_UP may similarly arrive before PDELAY_RESP.
	pendingPDelayFUs []pendingPDelayFollowUp

	statusMu      sync.RWMutex
	lastOffset    time.Duration
	meanPathDelay time.Duration
	frequencyPPB  float64
	servoState    ServoState
	lastSync      time.Time
	syncCount     uint64
}

// New creates a PTP client.
func New(
	tr ptpport.Transport,
	clk clock.Clock,
	selfID ptp.PortIdentity,
	cfg Config,
	log *slog.Logger,
) *Service {
	profile := cfg.Profile
	if cfg.DomainNumber != nil {
		profile.DomainNumber = *cfg.DomainNumber
	}
	if profile.C37238Version != ptp.C37238Disabled {
		profile.DelayMechanism = ptp.DelayMechanismP2P
	} else if cfg.DelayMechanism != 0 {
		profile.DelayMechanism = cfg.DelayMechanism
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.TimestampMode = tr.TimestampMode()

	servo := NewPIServo(PIServoConfig{
		MaxFrequency:       clk.MaxFreqAdj(),
		StepThreshold:      1.0,     // repeat a step when offset > 1 s
		FirstStepThreshold: 0.00002, // initial step when offset > 20 us
		SoftwareTS:         cfg.TimestampMode != "hardware",
	})
	servo.SyncInterval(profile.SyncInterval())

	s := &Service{
		cfg:            cfg,
		profile:        profile,
		tr:             tr,
		clk:            clk,
		servo:          servo,
		log:            log,
		selfID:         selfID,
		foreignMasters: make(map[ptp.PortIdentity]*ForeignMaster),
	}
	s.state.Store(uint32(StateInitializing))
	return s
}

// Run starts the PTP client and blocks until the context is canceled.
func (s *Service) Run(ctx context.Context) (runErr error) {
	s.tr.Start()
	defer s.tr.Close()
	defer func() {
		s.setClockSynchronized(false)
		if runErr != nil {
			s.state.Store(uint32(StateFaulty))
		} else {
			s.state.Store(uint32(StateStopped))
		}
	}()

	s.transition(EventInitComplete)
	s.log.Debug("ptp_client started",
		"profile", s.profile.Name,
		"domain", s.profile.DomainNumber,
		"transport", s.cfg.Transport,
		"delay_mechanism", delayMechName(s.profile.DelayMechanism),
		"c37_238_version", s.profile.C37238Version.String(),
		"self_id", s.selfID.String(),
	)

	checkInterval := time.Duration(s.profile.AnnounceInterval() * float64(time.Second))
	if syncInterval := time.Duration(s.profile.SyncInterval() * float64(time.Second)); syncInterval < checkInterval {
		checkInterval = syncInterval
	}
	announceTick := time.NewTicker(checkInterval)
	defer announceTick.Stop()

	delayTick := time.NewTicker(time.Duration(s.profile.DelayReqInterval() * float64(time.Second)))
	defer delayTick.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Debug("ptp_client stopping")
			return nil

		case pkt := <-s.tr.EventCh():
			s.handleEvent(pkt)

		case pkt := <-s.tr.GeneralCh():
			s.handleGeneral(pkt)

		case err, ok := <-s.tr.Errors():
			if !ok {
				return fmt.Errorf("ptp_client: transport stopped")
			}
			if err != nil {
				return err
			}

		case <-announceTick.C:
			s.checkTimeouts()

		case <-delayTick.C:
			s.sendDelayRequest()
		}
	}
}

func (s *Service) handleEvent(pkt ptpport.Packet) {
	if pkt.Timestamp.IsZero() {
		s.log.Warn("ptp_client: ignored event without transport timestamp")
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
	if hdr.SourcePortIdentity == s.selfID {
		return
	}

	body := pkt.Data[ptp.HeaderSize:int(hdr.MessageLength)]

	switch hdr.MessageType {
	case ptp.MsgSync:
		s.handleSync(hdr, body, pkt.Timestamp)
	case ptp.MsgPDelayReq:
		s.handlePDelayReq(hdr, body, pkt)
	case ptp.MsgPDelayResp:
		s.handlePDelayResp(hdr, body, pkt.Timestamp)
	}
}

func (s *Service) handleGeneral(pkt ptpport.Packet) {
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
	if hdr.SourcePortIdentity == s.selfID {
		return
	}

	body := pkt.Data[ptp.HeaderSize:int(hdr.MessageLength)]

	switch hdr.MessageType {
	case ptp.MsgAnnounce:
		s.handleAnnounce(hdr, body)
	case ptp.MsgFollowUp:
		s.handleFollowUp(hdr, body)
	case ptp.MsgDelayResp:
		s.handleDelayResp(hdr, body)
	case ptp.MsgPDelayRespFollowUp:
		s.handlePDelayRespFollowUp(hdr, body)
	}
}

func (s *Service) handleAnnounce(hdr ptp.Header, body []byte) {
	ann, err := ptp.DecodeAnnounceBody(body)
	if err != nil {
		s.log.Warn("ptp_client: bad announce", "err", err)
		return
	}
	if !announceProvidesAbsoluteUTC(hdr) {
		s.rejectMasterWithoutAbsoluteUTC(hdr)
		return
	}
	var profileTLVs ptp.AnnounceProfileTLVs
	if s.profile.C37238Version != ptp.C37238Disabled {
		profileTLVs, err = ptp.DecodeAnnounceProfileTLVs(body)
		if err != nil {
			s.rejectMaster(hdr, "ptp_client: rejecting malformed Power Profile Announce", "err", err)
			return
		}
		if profileTLVs.C37238Version != s.profile.C37238Version {
			s.rejectMaster(hdr, "ptp_client: rejecting incompatible Power Profile Announce",
				"expected_version", s.profile.C37238Version.String(),
				"received_version", profileTLVs.C37238Version.String(),
			)
			return
		}
	}

	now := time.Now()
	s.mu.Lock()
	fm, exists := s.foreignMasters[hdr.SourcePortIdentity]
	if !exists {
		fm = &ForeignMaster{Identity: hdr.SourcePortIdentity}
		s.foreignMasters[hdr.SourcePortIdentity] = fm
		s.log.Info("ptp_client: new foreign master",
			"source", hdr.SourcePortIdentity.String(),
			"gm", ann.GrandmasterIdentity.String(),
			"class", ann.GrandmasterClockQuality.ClockClass,
			"priority1", ann.GrandmasterPriority1,
		)
	}
	if !fm.recordAnnounce(now, s.announceQualificationWindow(hdr.LogMessageInterval), hdr.SequenceID) {
		s.mu.Unlock()
		return
	}
	fm.Announce = ann
	fm.Header = hdr
	fm.ProfileTLVs = profileTLVs
	fm.UTCOffset = ann.CurrentUtcOffset

	// BMCA: select the best master.
	prevBest := s.bestMaster
	best := s.selectBestMasterLocked()
	if best != nil {
		s.utcOffset = best.UTCOffset
	} else {
		s.utcOffset = 0
	}
	s.mu.Unlock()

	if best == nil {
		return
	}

	if prevBest != nil && best.Identity != prevBest.Identity {
		s.masterChanged(prevBest, best)
		return
	}
	if prevBest == nil && s.State() == StateListening {
		s.transition(EventRSSlave)
	}
}

func announceProvidesAbsoluteUTC(hdr ptp.Header) bool {
	const required = ptp.FlagPTPTimescale | ptp.FlagCurrentUtcOffsetValid
	return hdr.FlagField&required == required
}

// rejectMasterWithoutAbsoluteUTC prevents an arbitrary-timescale or
// UTC-offset-unknown master from driving the application's absolute UTC clock.
// If a selected master withdraws either capability, disqualify it immediately
// instead of continuing to discipline the clock until the Announce timeout.
func (s *Service) rejectMasterWithoutAbsoluteUTC(hdr ptp.Header) {
	s.rejectMaster(hdr, "ptp_client: rejecting master without absolute UTC timescale",
		"ptp_timescale", hdr.FlagField&ptp.FlagPTPTimescale != 0,
		"utc_offset_valid", hdr.FlagField&ptp.FlagCurrentUtcOffsetValid != 0,
	)
}

// rejectMaster removes a source immediately when its current Announce is not
// compatible with the configured client profile. This also withdraws a
// selected master without waiting for the Announce receipt timeout.
func (s *Service) rejectMaster(hdr ptp.Header, message string, attrs ...any) {
	attrs = append([]any{"source", hdr.SourcePortIdentity.String()}, attrs...)
	s.log.Warn(message, attrs...)

	s.mu.Lock()
	previousBest := s.bestMaster
	delete(s.foreignMasters, hdr.SourcePortIdentity)
	best := s.selectBestMasterLocked()
	if best != nil {
		s.utcOffset = best.UTCOffset
	} else {
		s.utcOffset = 0
	}
	s.mu.Unlock()

	if previousBest == nil || previousBest.Identity != hdr.SourcePortIdentity {
		return
	}
	if best != nil {
		s.masterChanged(previousBest, best)
		return
	}

	s.servo.Reset()
	s.tsproc.Reset()
	s.delayReqPending = false
	s.pdelayReqPending = false
	s.syncReceived = false
	s.clearPendingFollowUps()
	s.clearPendingPDelayFollowUps()
	s.setClockSynchronized(false)
	s.transition(EventAnnounceTimeout)
}

func (s *Service) announceQualificationWindow(logInterval int8) time.Duration {
	interval := time.Duration(s.profile.AnnounceInterval() * float64(time.Second))
	if logInterval != 0x7F && logInterval >= -30 && logInterval <= 30 {
		interval = time.Duration(math.Ldexp(1, int(logInterval)) * float64(time.Second))
	}
	if interval <= 0 {
		interval = time.Second
	}
	return foreignMasterTimeWindow * interval
}

// selectBestMasterLocked runs BMCA over qualified foreign masters. s.mu must
// be held by the caller.
func (s *Service) selectBestMasterLocked() *ForeignMaster {
	masters := make([]*ForeignMaster, 0, len(s.foreignMasters))
	for _, master := range s.foreignMasters {
		if master.qualified() {
			masters = append(masters, master)
		}
	}
	best := SelectBestMaster(masters, s.selfID)
	s.bestMaster = best
	return best
}

func (s *Service) masterChanged(oldMaster, newMaster *ForeignMaster) {
	s.log.Info("ptp_client: master changed",
		"old", oldMaster.Identity.String(),
		"new", newMaster.Identity.String(),
	)
	s.servo.Reset()
	s.tsproc.Reset()
	s.syncReceived = false
	s.clearPendingFollowUps()
	s.setClockSynchronized(false)
	s.transition(EventMasterChanged)
}

func (s *Service) handleSync(hdr ptp.Header, body []byte, rxTime time.Time) {
	state := s.State()
	if state != StateUncalibrated && state != StateSlave {
		return
	}

	s.mu.Lock()
	best := s.bestMaster
	s.mu.Unlock()
	if best == nil || hdr.SourcePortIdentity != best.Identity {
		return
	}

	s.tsproc.SetT2(s.clk.FromSystem(rxTime))
	s.tsproc.SetCorrectionSync(hdr.CorrectionField)
	s.lastSyncSeq = hdr.SequenceID
	s.twoStep.Store(hdr.FlagField&ptp.FlagTwoStep != 0)

	if !s.twoStep.Load() {
		// One-step: the origin timestamp is carried in Sync itself.
		syncBody, err := ptp.DecodeSyncBody(body)
		if err != nil {
			return
		}
		s.tsproc.SetT1(s.ptpTimestampToTime(syncBody.OriginTimestamp))
		s.tsproc.SetCorrectionFollowUp(0)
		s.processSyncComplete()
		return
	}
	s.syncReceived = true
	if followUp, ok := s.takePendingFollowUp(hdr.SourcePortIdentity, hdr.SequenceID); ok {
		s.completeFollowUp(followUp.header, followUp.body)
	}
}

func (s *Service) handleFollowUp(hdr ptp.Header, body []byte) {
	state := s.State()
	if state != StateUncalibrated && state != StateSlave {
		return
	}

	s.mu.Lock()
	best := s.bestMaster
	s.mu.Unlock()
	if best == nil || hdr.SourcePortIdentity != best.Identity {
		return
	}

	fu, err := ptp.DecodeFollowUpBody(body)
	if err != nil {
		return
	}
	if !s.syncReceived || !s.twoStep.Load() || hdr.SequenceID != s.lastSyncSeq {
		s.storePendingFollowUp(pendingFollowUp{header: hdr, body: fu})
		return
	}
	s.completeFollowUp(hdr, fu)
}

func (s *Service) completeFollowUp(hdr ptp.Header, fu ptp.FollowUpBody) {
	s.tsproc.SetT1(s.ptpTimestampToTime(fu.PreciseOriginTimestamp))
	s.tsproc.SetCorrectionFollowUp(hdr.CorrectionField)
	s.processSyncComplete()
}

func (s *Service) storePendingFollowUp(followUp pendingFollowUp) {
	for i := range s.pendingFollowUps {
		pending := &s.pendingFollowUps[i]
		if pending.header.SourcePortIdentity == followUp.header.SourcePortIdentity &&
			pending.header.SequenceID == followUp.header.SequenceID {
			*pending = followUp
			return
		}
	}
	if len(s.pendingFollowUps) == pendingFollowUpLimit {
		copy(s.pendingFollowUps, s.pendingFollowUps[1:])
		s.pendingFollowUps[len(s.pendingFollowUps)-1] = followUp
		return
	}
	s.pendingFollowUps = append(s.pendingFollowUps, followUp)
}

func (s *Service) takePendingFollowUp(source ptp.PortIdentity, sequenceID uint16) (pendingFollowUp, bool) {
	for i, followUp := range s.pendingFollowUps {
		if followUp.header.SourcePortIdentity != source || followUp.header.SequenceID != sequenceID {
			continue
		}
		copy(s.pendingFollowUps[i:], s.pendingFollowUps[i+1:])
		last := len(s.pendingFollowUps) - 1
		s.pendingFollowUps[last] = pendingFollowUp{}
		s.pendingFollowUps = s.pendingFollowUps[:last]
		return followUp, true
	}
	return pendingFollowUp{}, false
}

func (s *Service) clearPendingFollowUps() {
	clear(s.pendingFollowUps)
	s.pendingFollowUps = nil
}

func (s *Service) processSyncComplete() {
	var offset time.Duration
	var meanPathDelay time.Duration
	var ok bool

	if s.profile.DelayMechanism == ptp.DelayMechanismP2P {
		offset, ok = s.tsproc.OffsetP2P()
		meanPathDelay = s.tsproc.peerDelay
	} else {
		offset, meanPathDelay, ok = s.tsproc.OffsetE2E()
	}

	if !ok {
		return
	}
	s.statusMu.Lock()
	s.lastOffset = offset
	s.meanPathDelay = meanPathDelay
	s.lastSync = time.Now()
	s.syncCount++
	s.statusMu.Unlock()
	s.disciplineClock(offset)
	s.syncReceived = false
}

func (s *Service) disciplineClock(offset time.Duration) {
	// TsProc returns local-minus-master offset. The clock actuator and the
	// PI servo expect the correction to apply to the local clock,
	// therefore the sign must be inverted.
	correction := -offset
	localNs := uint64(s.clk.Now().UnixNano())
	ppb, servoState := s.servo.Sample(correction.Nanoseconds(), localNs)
	s.statusMu.Lock()
	s.servoState = servoState
	s.frequencyPPB = ppb
	s.statusMu.Unlock()

	switch servoState {
	case ServoJump:
		if err := s.clk.Step(correction); err != nil {
			s.log.Error("ptp_client: clock step failed", "err", err)
			s.setClockSynchronized(false)
		} else {
			// Every local timestamp collected before the step belongs to the old
			// application-clock scale. Keep the already measured P2P peer delay,
			// but invalidate in-flight timestamp exchanges; E2E must wait for a
			// fresh DelayReq/DelayResp before producing another offset.
			s.tsproc.Reset()
			s.delayReqPending = false
			s.pdelayReqPending = false
			s.clearPendingFollowUps()
			s.clearPendingPDelayFollowUps()
			s.setClockSynchronized(true)
			s.log.Info("ptp_client: clock step",
				"offset_ns", offset.Nanoseconds(),
				"correction_ns", correction.Nanoseconds(),
			)
		}
		if s.State() == StateUncalibrated {
			s.transition(EventMasterClockSelected)
		}

	case ServoLocked, ServoLockedStable:
		if err := s.clk.AdjustFrequency(ppb); err != nil {
			s.log.Error("ptp_client: freq adjust failed", "err", err)
			s.setClockSynchronized(false)
			break
		}
		s.setClockSynchronized(true)
		if s.State() == StateUncalibrated {
			s.transition(EventMasterClockSelected)
		}
		s.log.Debug("ptp_client: sync",
			"state", s.State().String(),
			"servo", servoState.String(),
			"offset_ns", offset.Nanoseconds(),
			"freq_ppb", int64(math.Round(ppb)),
		)

	case ServoUnlocked:
		s.setClockSynchronized(false)
		s.log.Debug("ptp_client: servo unlocked",
			"offset_ns", offset.Nanoseconds(),
		)
	}
}

func (s *Service) sendDelayRequest() {
	state := s.State()
	if state != StateUncalibrated && state != StateSlave {
		return
	}

	if s.profile.DelayMechanism == ptp.DelayMechanismP2P {
		s.sendPDelayReq()
	} else {
		s.sendDelayReq()
	}
}

func (s *Service) sendDelayReq() {
	hdr := ptp.Header{
		MessageType:        ptp.MsgDelayReq,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       s.profile.DomainNumber,
		SourcePortIdentity: s.selfID,
		SequenceID:         s.delayReqSeq,
		ControlField:       ptp.ControlDelayReq,
		LogMessageInterval: 0x7F, // -128..127, 0x7F = no rate
	}
	body := ptp.DelayReqBody{
		OriginTimestamp: ptp.TimestampFromTime(s.clk.Now()),
	}
	data := ptp.EncodeDelayReq(hdr, body)
	txTime, err := s.tr.SendEvent(data)
	if err != nil {
		s.log.Warn("ptp_client: send delay_req failed", "err", err)
		return
	}
	if txTime.IsZero() {
		s.log.Warn("ptp_client: send delay_req failed", "err", "transport returned an empty TX timestamp")
		return
	}
	s.tsproc.SetT3(s.clk.FromSystem(txTime))
	s.tsproc.SetT4(time.Time{})
	s.lastDelayReqSeq = hdr.SequenceID
	s.delayReqPending = true
	s.delayReqSeq++
}

func (s *Service) sendPDelayReq() {
	hdr := ptp.Header{
		MessageType:        ptp.MsgPDelayReq,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       s.profile.DomainNumber,
		SourcePortIdentity: s.selfID,
		SequenceID:         s.pdelayReqSeq,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	body := ptp.PDelayReqBody{
		OriginTimestamp: ptp.TimestampFromTime(s.clk.Now()),
	}
	data := ptp.EncodePDelayReq(hdr, body)
	txTime, err := s.tr.SendEvent(data)
	if err != nil {
		s.log.Warn("ptp_client: send pdelay_req failed", "err", err)
		return
	}
	if txTime.IsZero() {
		s.log.Warn("ptp_client: send pdelay_req failed", "err", "transport returned an empty TX timestamp")
		return
	}
	s.tsproc.SetT3(s.clk.FromSystem(txTime)) // t1 in the pdelay context
	s.pdelayT1 = time.Time{}
	s.pdelayT2 = time.Time{}
	s.pdelayT3 = time.Time{}
	s.pdelayT4 = time.Time{}
	s.pdelayRespCorrection = 0
	s.pdelayRespReceived = false
	s.pdelayRespSource = ptp.PortIdentity{}
	s.clearPendingPDelayFollowUps()
	s.lastPDelayReqSeq = hdr.SequenceID
	s.pdelayReqPending = true
	s.pdelayReqSeq++
}

// handlePDelayReq makes the slave-only port a complete P2P peer. A Power
// Profile Grandmaster may measure the link in the opposite direction, so the
// client responds with the same two-step exchange as the server port.
func (s *Service) handlePDelayReq(reqHdr ptp.Header, body []byte, pkt ptpport.Packet) {
	if s.profile.DelayMechanism != ptp.DelayMechanismP2P {
		return
	}
	state := s.State()
	if state != StateListening && state != StateUncalibrated && state != StateSlave {
		return
	}
	if _, err := ptp.DecodePDelayReqBody(body); err != nil {
		return
	}

	unicast := reqHdr.FlagField&ptp.FlagUnicast != 0
	flags := uint16(ptp.FlagTwoStep)
	if unicast {
		flags |= ptp.FlagUnicast
	}
	respHeader := ptp.Header{
		MessageType:        ptp.MsgPDelayResp,
		TransportSpecific:  reqHdr.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       reqHdr.DomainNumber,
		FlagField:          flags,
		SourcePortIdentity: s.selfID,
		SequenceID:         reqHdr.SequenceID,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	respBody := ptp.PDelayRespBody{
		RequestReceiptTimestamp: s.systemTimeToPTPTimestamp(pkt.Timestamp),
		RequestingPortIdentity:  reqHdr.SourcePortIdentity,
	}
	respData := ptp.EncodePDelayResp(respHeader, respBody)

	var txTime time.Time
	var err error
	if unicast {
		txTime, err = s.tr.SendEventTo(respData, pkt.From)
	} else {
		txTime, err = s.tr.SendEvent(respData)
	}
	if err != nil {
		s.log.Warn("ptp_client: send PDELAY_RESP failed", "err", err)
		return
	}
	if txTime.IsZero() {
		s.log.Warn("ptp_client: send PDELAY_RESP failed", "err", "transport returned an empty TX timestamp")
		return
	}

	followUpHeader := ptp.Header{
		MessageType:        ptp.MsgPDelayRespFollowUp,
		TransportSpecific:  s.profile.TransportSpecific,
		VersionPTP:         ptp.PTPVersion2,
		DomainNumber:       reqHdr.DomainNumber,
		FlagField:          flags & ptp.FlagUnicast,
		CorrectionField:    reqHdr.CorrectionField,
		SourcePortIdentity: s.selfID,
		SequenceID:         reqHdr.SequenceID,
		ControlField:       ptp.ControlOther,
		LogMessageInterval: 0x7F,
	}
	followUpBody := ptp.PDelayRespFollowUpBody{
		ResponseOriginTimestamp: s.systemTimeToPTPTimestamp(txTime),
		RequestingPortIdentity:  reqHdr.SourcePortIdentity,
	}
	followUpData := ptp.EncodePDelayRespFollowUp(followUpHeader, followUpBody)
	if unicast {
		err = s.tr.SendGeneralTo(followUpData, pkt.From)
	} else {
		err = s.tr.SendGeneral(followUpData)
	}
	if err != nil {
		s.log.Warn("ptp_client: send PDELAY_RESP_FOLLOW_UP failed", "err", err)
		return
	}

	s.pdelayRespTx.Add(1)
	s.log.Debug("ptp_client: TX PDELAY_RESP+FOLLOW_UP",
		"seq", reqHdr.SequenceID,
		"from", reqHdr.SourcePortIdentity.String(),
	)
}

func (s *Service) handleDelayResp(hdr ptp.Header, body []byte) {
	state := s.State()
	if state != StateUncalibrated && state != StateSlave {
		return
	}
	resp, err := ptp.DecodeDelayRespBody(body)
	if err != nil {
		return
	}
	// Verify that the response is addressed to us.
	if resp.RequestingPortIdentity != s.selfID {
		return
	}
	s.mu.Lock()
	best := s.bestMaster
	s.mu.Unlock()
	if best == nil || hdr.SourcePortIdentity != best.Identity {
		return
	}
	if !s.delayReqPending || hdr.SequenceID != s.lastDelayReqSeq {
		return
	}
	s.tsproc.SetT4(s.ptpTimestampToTime(resp.ReceiveTimestamp))
	s.tsproc.SetCorrectionDelayResp(hdr.CorrectionField)
	s.delayReqPending = false
}

func (s *Service) handlePDelayResp(hdr ptp.Header, body []byte, rxTime time.Time) {
	if s.profile.DelayMechanism != ptp.DelayMechanismP2P {
		return
	}
	resp, err := ptp.DecodePDelayRespBody(body)
	if err != nil {
		return
	}
	if resp.RequestingPortIdentity != s.selfID {
		return
	}
	if !s.pdelayReqPending || hdr.SequenceID != s.lastPDelayReqSeq {
		return
	}

	s.pdelayT4 = s.clk.FromSystem(rxTime)
	s.pdelayT2 = s.ptpTimestampToTime(resp.RequestReceiptTimestamp)
	s.pdelayT1 = s.tsproc.t3
	s.pdelayT3 = time.Time{}
	s.pdelayRespSeq = hdr.SequenceID
	s.pdelayRespCorrection = hdr.CorrectionField
	s.pdelayRespReceived = true
	s.pdelayRespSource = hdr.SourcePortIdentity

	if hdr.FlagField&ptp.FlagTwoStep == 0 {
		s.computePeerDelay(s.pdelayRespCorrection)
		s.pdelayReqPending = false
		s.pdelayRespReceived = false
		return
	}
	if followUp, ok := s.takePendingPDelayFollowUp(hdr.SourcePortIdentity, hdr.SequenceID); ok {
		s.completePDelayFollowUp(followUp.header, followUp.body)
	}
}

func (s *Service) handlePDelayRespFollowUp(hdr ptp.Header, body []byte) {
	if s.profile.DelayMechanism != ptp.DelayMechanismP2P {
		return
	}
	fu, err := ptp.DecodePDelayRespFollowUpBody(body)
	if err != nil {
		return
	}
	if fu.RequestingPortIdentity != s.selfID {
		return
	}
	if !s.pdelayReqPending {
		return
	}
	if hdr.SequenceID != s.lastPDelayReqSeq {
		return
	}
	if !s.pdelayRespReceived || hdr.SequenceID != s.pdelayRespSeq || hdr.SourcePortIdentity != s.pdelayRespSource {
		s.storePendingPDelayFollowUp(pendingPDelayFollowUp{header: hdr, body: fu})
		return
	}
	s.completePDelayFollowUp(hdr, fu)
}

func (s *Service) completePDelayFollowUp(hdr ptp.Header, fu ptp.PDelayRespFollowUpBody) {
	s.pdelayT3 = s.ptpTimestampToTime(fu.ResponseOriginTimestamp)
	s.computePeerDelay(s.pdelayRespCorrection + hdr.CorrectionField)
	s.pdelayReqPending = false
	s.pdelayRespReceived = false
}

func (s *Service) storePendingPDelayFollowUp(followUp pendingPDelayFollowUp) {
	for i := range s.pendingPDelayFUs {
		pending := &s.pendingPDelayFUs[i]
		if pending.header.SourcePortIdentity == followUp.header.SourcePortIdentity &&
			pending.header.SequenceID == followUp.header.SequenceID {
			*pending = followUp
			return
		}
	}
	if len(s.pendingPDelayFUs) == pendingFollowUpLimit {
		copy(s.pendingPDelayFUs, s.pendingPDelayFUs[1:])
		s.pendingPDelayFUs[len(s.pendingPDelayFUs)-1] = followUp
		return
	}
	s.pendingPDelayFUs = append(s.pendingPDelayFUs, followUp)
}

func (s *Service) takePendingPDelayFollowUp(source ptp.PortIdentity, sequenceID uint16) (pendingPDelayFollowUp, bool) {
	for i, followUp := range s.pendingPDelayFUs {
		if followUp.header.SourcePortIdentity != source || followUp.header.SequenceID != sequenceID {
			continue
		}
		copy(s.pendingPDelayFUs[i:], s.pendingPDelayFUs[i+1:])
		last := len(s.pendingPDelayFUs) - 1
		s.pendingPDelayFUs[last] = pendingPDelayFollowUp{}
		s.pendingPDelayFUs = s.pendingPDelayFUs[:last]
		return followUp, true
	}
	return pendingPDelayFollowUp{}, false
}

func (s *Service) clearPendingPDelayFollowUps() {
	clear(s.pendingPDelayFUs)
	s.pendingPDelayFUs = nil
}

func (s *Service) ptpTimestampToTime(ts ptp.Timestamp) time.Time {
	s.mu.Lock()
	offset := s.utcOffset
	s.mu.Unlock()
	return ts.ToTimeWithUTCOffset(offset)
}

func (s *Service) systemTimeToPTPTimestamp(systemTime time.Time) ptp.Timestamp {
	s.mu.Lock()
	offset := s.utcOffset
	s.mu.Unlock()
	return ptp.TimestampFromTimeWithUTCOffset(s.clk.FromSystem(systemTime), offset)
}

func (s *Service) computePeerDelay(corrField int64) {
	if s.pdelayT1.IsZero() || s.pdelayT2.IsZero() || s.pdelayT4.IsZero() {
		return
	}
	t3 := s.pdelayT3
	if t3.IsZero() {
		t3 = s.pdelayT2
	}
	d41 := s.pdelayT4.Sub(s.pdelayT1)
	d32 := t3.Sub(s.pdelayT2)
	corr := correctionDuration(corrField)
	peerDelay := (d41 - d32 - corr) / 2

	if peerDelay < 0 {
		peerDelay = 0
	}

	s.tsproc.SetPeerDelay(peerDelay)
	s.statusMu.Lock()
	s.meanPathDelay = peerDelay
	s.statusMu.Unlock()

	s.log.Debug("ptp_client: peer delay",
		"delay_ns", peerDelay.Nanoseconds(),
	)
}

func (s *Service) checkTimeouts() {
	now := time.Now()
	s.mu.Lock()
	previousBest := s.bestMaster
	timeout := time.Duration(s.profile.AnnounceTimeoutDuration() * float64(time.Second))
	for id, fm := range s.foreignMasters {
		if now.Sub(fm.LastSeen) > timeout {
			s.log.Warn("ptp_client: foreign master timeout",
				"source", id.String(),
			)
			delete(s.foreignMasters, id)
		}
	}
	best := s.selectBestMasterLocked()
	if best != nil {
		s.utcOffset = best.UTCOffset
	} else {
		s.utcOffset = 0
	}
	s.mu.Unlock()

	if previousBest != nil && best == nil {
		s.log.Warn("ptp_client: no foreign masters, returning to LISTENING")
		s.servo.Reset()
		s.tsproc.Reset()
		s.syncReceived = false
		s.clearPendingFollowUps()
		s.setClockSynchronized(false)
		s.transition(EventAnnounceTimeout)
		return
	}
	if previousBest != nil && best != nil && previousBest.Identity != best.Identity {
		s.masterChanged(previousBest, best)
		return
	}

	s.statusMu.RLock()
	lastSync := s.lastSync
	s.statusMu.RUnlock()
	syncTimeout := time.Duration(3 * s.profile.SyncInterval() * float64(time.Second))
	if s.State() == StateSlave && !lastSync.IsZero() && now.Sub(lastSync) > syncTimeout {
		s.log.Warn("ptp_client: sync timeout, returning to UNCALIBRATED",
			"last_sync", lastSync,
			"timeout", syncTimeout,
		)
		s.servo.Reset()
		s.tsproc.Reset()
		s.syncReceived = false
		s.clearPendingFollowUps()
		s.setClockSynchronized(false)
		s.transition(EventSyncFault)
	}
}

func (s *Service) transition(event FSMEvent) {
	oldState := s.State()
	newState := SlavePortFSM(oldState, event)
	if newState != oldState {
		s.log.Info("ptp_client: port state",
			"old", oldState.String(),
			"new", newState.String(),
			"event", fsmEventName(event),
		)
		s.state.Store(uint32(newState))
	}
}

func (s *Service) setClockSynchronized(synchronized bool) {
	if controller, ok := s.clk.(clock.SyncController); ok {
		controller.SetSynchronized(synchronized)
	}
}

// State returns the current port state.
func (s *Service) State() PortState { return PortState(s.state.Load()) }

// Snapshot returns a consistent snapshot of the PTP Client state.
func (s *Service) Snapshot() Snapshot {
	var masterPort ptp.PortIdentity
	var grandmaster ptp.ClockIdentity
	var masterC37238Version ptp.C37238Version
	var c37238GrandmasterID uint16
	var grandmasterTimeInaccuracy uint32
	var networkTimeInaccuracy uint32
	var totalTimeInaccuracy uint32
	var alternateTimeOffset *ptp.AlternateTimeOffsetTLV
	s.mu.Lock()
	if s.bestMaster != nil {
		masterPort = s.bestMaster.Identity
		grandmaster = s.bestMaster.Announce.GrandmasterIdentity
		profileTLVs := s.bestMaster.ProfileTLVs
		masterC37238Version = profileTLVs.C37238Version
		if profileTLVs.C372382011 != nil {
			c37238GrandmasterID = profileTLVs.C372382011.GrandmasterID
			grandmasterTimeInaccuracy = profileTLVs.C372382011.GrandmasterTimeInaccuracy
			networkTimeInaccuracy = profileTLVs.C372382011.NetworkTimeInaccuracy
		}
		if profileTLVs.C372382017 != nil {
			c37238GrandmasterID = profileTLVs.C372382017.GrandmasterID
			totalTimeInaccuracy = profileTLVs.C372382017.TotalTimeInaccuracy
		}
		if profileTLVs.AlternateTimeOffset != nil {
			alternate := *profileTLVs.AlternateTimeOffset
			alternateTimeOffset = &alternate
		}
	}
	utcOffset := s.utcOffset
	s.mu.Unlock()

	s.statusMu.RLock()
	offset := s.lastOffset
	meanPathDelay := s.meanPathDelay
	frequencyPPB := s.frequencyPPB
	servoState := s.servoState
	lastSync := s.lastSync
	syncCount := s.syncCount
	s.statusMu.RUnlock()

	return Snapshot{
		Source:                    "external",
		State:                     s.State(),
		Profile:                   s.profile.Name,
		DelayMechanism:            delayMechName(s.profile.DelayMechanism),
		DomainNumber:              s.profile.DomainNumber,
		Transport:                 s.cfg.Transport,
		TimestampMode:             s.cfg.TimestampMode,
		LocalPortIdentity:         s.selfID,
		MasterPortIdentity:        masterPort,
		GrandmasterIdentity:       grandmaster,
		UTCOffset:                 utcOffset,
		ExpectedC37238Version:     s.profile.C37238Version,
		MasterC37238Version:       masterC37238Version,
		C37238GrandmasterID:       c37238GrandmasterID,
		GrandmasterTimeInaccuracy: grandmasterTimeInaccuracy,
		NetworkTimeInaccuracy:     networkTimeInaccuracy,
		TotalTimeInaccuracy:       totalTimeInaccuracy,
		AlternateTimeOffset:       alternateTimeOffset,
		PDelayRespCount:           s.pdelayRespTx.Load(),
		TwoStep:                   s.twoStep.Load(),
		Offset:                    offset,
		MeanPathDelay:             meanPathDelay,
		FrequencyPPB:              frequencyPPB,
		ServoState:                servoState,
		LastSync:                  lastSync,
		SyncCount:                 syncCount,
		ApplicationTime:           s.clk.Now(),
		ClockStatus:               clock.StatusOf(s.clk),
	}
}

func delayMechName(dm uint8) string {
	switch dm {
	case ptp.DelayMechanismE2E:
		return "E2E"
	case ptp.DelayMechanismP2P:
		return "P2P"
	default:
		return "unknown"
	}
}

func fsmEventName(e FSMEvent) string {
	switch e {
	case EventInitComplete:
		return "INIT_COMPLETE"
	case EventRSSlave:
		return "RS_SLAVE"
	case EventMasterClockSelected:
		return "MASTER_CLOCK_SELECTED"
	case EventAnnounceTimeout:
		return "ANNOUNCE_TIMEOUT"
	case EventSyncFault:
		return "SYNC_FAULT"
	case EventFault:
		return "FAULT"
	case EventFaultCleared:
		return "FAULT_CLEARED"
	case EventMasterChanged:
		return "MASTER_CHANGED"
	default:
		return "UNKNOWN"
	}
}
