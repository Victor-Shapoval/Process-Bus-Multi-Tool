package goosepub

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

// PublisherConfig configures one GOOSE publisher.
type PublisherConfig struct {
	Name    string
	DstMAC  net.HardwareAddr
	SrcMAC  net.HardwareAddr
	AppID   uint16
	GocbRef string
	DatSet  string
	GoID    string
	ConfRev uint32
	NdsCom  bool
	VLAN    *ethernet.VLANTag // nil = untagged
	MinMs   uint32            // rapid retransmissions after a state change
	MaxMs   uint32            // TimeAllowedToLive / heartbeat period

	// Initial data set
	InitialData []goose.DataValue
}

// Service publishes GOOSE messages.
// It manages stNum/sqNum, heartbeats, and burst retransmissions
// according to IEC 61850-8-1 Section 8.2.
type Service struct {
	cfg  PublisherConfig
	sink FrameSink
	log  *slog.Logger

	mu            sync.Mutex
	stNum         uint32
	sqNum         uint32
	logicalData   []goose.DataValue
	data          []goose.DataValue
	test          bool
	ndsCom        bool
	sim           bool
	stateChanged  time.Time
	timeQuality   goose.TimeQuality
	timeSource    appclock.Source
	stateChangeCh chan struct{}
}

// New creates a GOOSE publisher. source must be the same application clock
// disciplined by the built-in PTP Client. This preserves one timescale when
// working with either an external or a local PTP Server.
// When PTP is disabled, ApplicationClock falls back to system time itself.
func New(cfg PublisherConfig, sink FrameSink, log *slog.Logger, source appclock.Source) *Service {
	if cfg.MinMs == 0 {
		cfg.MinMs = 2
	}
	if cfg.MaxMs == 0 {
		cfg.MaxMs = 1000
	}
	if cfg.MinMs > cfg.MaxMs {
		cfg.MinMs = cfg.MaxMs
	}

	if log == nil {
		log = slog.Default()
	}
	if source == nil {
		source = appclock.SystemSource{}
	}
	stateChanged := source.Now().UTC()
	timeQuality := gooseTimeQuality(source)
	return &Service{
		cfg:           cfg,
		sink:          sink,
		log:           log,
		stNum:         1,
		logicalData:   cloneDataValues(cfg.InitialData),
		data:          resolveDataTimes(cfg.InitialData, stateChanged, timeQuality),
		ndsCom:        cfg.NdsCom,
		stateChanged:  stateChanged,
		timeQuality:   timeQuality,
		timeSource:    source,
		stateChangeCh: make(chan struct{}, 1),
	}
}

// ApplyState atomically applies the complete publisher state. When the state
// actually changes, stNum is incremented exactly once, sqNum is reset to 0,
// and the t field remains fixed until the next state change.
func (s *Service) ApplyState(data []goose.DataValue, test, simulation bool) bool {
	s.mu.Lock()
	if dataValuesEqual(s.logicalData, data) && s.test == test && s.sim == simulation {
		s.mu.Unlock()
		return false
	}
	stateChanged := s.timeSource.Now().UTC()
	timeQuality := gooseTimeQuality(s.timeSource)
	s.logicalData = cloneDataValues(data)
	s.data = resolveDataTimes(data, stateChanged, timeQuality)
	s.test = test
	s.sim = simulation
	s.stNum++
	if s.stNum == 0 {
		s.stNum = 1
	}
	s.sqNum = 0
	s.stateChanged = stateChanged
	s.timeQuality = timeQuality
	s.mu.Unlock()
	s.notifyStateChange()
	return true
}

// Run starts the GOOSE publication loop and blocks until the context is canceled.
//
// Retransmission logic (IEC 61850-8-1 Section 8.2):
//   - On a state change (stNum++), transmit immediately, then repeat at
//     MinMs, 2*MinMs, and so on up to MaxMs.
//   - In steady state, send a heartbeat every MaxMs.
func (s *Service) Run(ctx context.Context) error {
	minInterval := time.Duration(s.cfg.MinMs) * time.Millisecond
	maxInterval := time.Duration(s.cfg.MaxMs) * time.Millisecond

	s.log.Debug("goose_pub started",
		"name", s.cfg.Name,
		"dst_mac", s.cfg.DstMAC.String(),
		"app_id", s.cfg.AppID,
		"gocb_ref", s.cfg.GocbRef,
		"min_interval_ms", s.cfg.MinMs,
		"max_interval_ms", s.cfg.MaxMs,
	)

	// Send the first frame of the initial steady state.
	lastTransmittedStNum, err := s.transmitState()
	if err != nil {
		return err
	}

	timer := time.NewTimer(maxInterval)
	defer timer.Stop()
	nextInterval := maxInterval

	for {
		select {
		case <-ctx.Done():
			s.log.Debug("goose_pub stopping", "name", s.cfg.Name)
			return nil

		case <-s.stateChangeCh:
			// If the timer has already sent the new state, the notification only
			// restarts the intervals and does not create a duplicate before MinTime.
			if s.getStNum() != lastTransmittedStNum {
				lastTransmittedStNum, err = s.transmitState()
				if err != nil {
					return err
				}
			}
			nextInterval = minInterval
			resetTimer(timer, nextInterval)

		case <-timer.C:
			transmittedStNum, err := s.transmitState()
			if err != nil {
				return err
			}
			if transmittedStNum != lastTransmittedStNum {
				nextInterval = minInterval
			} else {
				nextInterval = nextGooseInterval(nextInterval, maxInterval)
			}
			lastTransmittedStNum = transmittedStNum
			timer.Reset(nextInterval)
		}
	}
}

func (s *Service) notifyStateChange() {
	select {
	case s.stateChangeCh <- struct{}{}:
	default:
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func nextGooseInterval(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func (s *Service) transmitState() (uint32, error) {
	s.mu.Lock()
	pdu := &goose.PDU{
		DstMAC:              s.cfg.DstMAC,
		SrcMAC:              s.cfg.SrcMAC,
		VLAN:                s.cfg.VLAN,
		AppID:               s.cfg.AppID,
		Simulation:          s.sim,
		GocbRef:             s.cfg.GocbRef,
		TimeAllowedToLiveMs: s.cfg.MaxMs,
		DatSet:              s.cfg.DatSet,
		GoID:                s.cfg.GoID,
		Timestamp:           s.stateChanged,
		TimeQuality:         s.timeQuality,
		StNum:               s.stNum,
		SqNum:               s.sqNum,
		Test:                s.test,
		ConfRev:             s.cfg.ConfRev,
		NdsCom:              s.ndsCom,
		NumDatSetEntries:    uint32(len(s.data)),
		AllData:             s.data,
	}
	transmittedStNum := s.stNum
	s.sqNum++
	if s.sqNum == 0 {
		s.sqNum = 1
	}
	s.mu.Unlock()

	frame := goose.Encode(pdu)
	if err := s.sink.WriteFrame(frame); err != nil {
		s.log.Warn("goose_pub: write frame failed",
			"name", s.cfg.Name,
			"err", err,
		)
		return transmittedStNum, err
	}
	return transmittedStNum, nil
}

func gooseTimeQuality(source appclock.Source) goose.TimeQuality {
	status := appclock.StatusOf(source)
	return goose.TimeQuality{
		ClockNotSynchronized: !status.PTPEnabled || !status.Synchronized,
		Accuracy:             goose.AccuracyUnspecified,
	}
}

func (s *Service) getStNum() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stNum
}

func resolveDataTimes(values []goose.DataValue, now time.Time, quality goose.TimeQuality) []goose.DataValue {
	resolved := make([]goose.DataValue, len(values))
	for i := range values {
		resolved[i] = values[i]
		resolved[i].Bytes = append([]byte(nil), values[i].Bytes...)
		if resolved[i].Type == goose.DataTypeUTCTime {
			if resolved[i].Time.IsZero() {
				resolved[i].Time = now
			}
			resolved[i].TimeQuality = quality
		}
		resolved[i].Children = resolveDataTimes(values[i].Children, now, quality)
	}
	return resolved
}

func cloneDataValues(values []goose.DataValue) []goose.DataValue {
	cloned := make([]goose.DataValue, len(values))
	for i := range values {
		cloned[i] = values[i]
		cloned[i].Bytes = append([]byte(nil), values[i].Bytes...)
		cloned[i].Children = cloneDataValues(values[i].Children)
	}
	return cloned
}

func dataValuesEqual(a, b []goose.DataValue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].Bool != b[i].Bool ||
			a[i].Int != b[i].Int ||
			a[i].UInt != b[i].UInt ||
			a[i].Float != b[i].Float ||
			a[i].String != b[i].String ||
			a[i].BitLength != b[i].BitLength ||
			a[i].TimeQuality != b[i].TimeQuality ||
			!a[i].Time.Equal(b[i].Time) ||
			!bytes.Equal(a[i].Bytes, b[i].Bytes) ||
			!dataValuesEqual(a[i].Children, b[i].Children) {
			return false
		}
	}
	return true
}
