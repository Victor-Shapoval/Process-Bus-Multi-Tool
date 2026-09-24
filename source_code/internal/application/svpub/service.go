package svpub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/sv"
)

// PublisherConfig configures one SV stream.
type PublisherConfig struct {
	Name                  string
	DstMAC                net.HardwareAddr
	SrcMAC                net.HardwareAddr
	AppID                 uint16
	SvID                  string
	DatSet                string // DataSet name for SCL/ICD; not transmitted in a 9-2LE ASDU
	ConfRev               uint32
	VLAN                  *ethernet.VLANTag // nil = untagged
	SmpRate               uint16            // samples per period (80 or 256)
	SampleTimingFrequency uint16            // SV frame timing frequency in Hz (50 or 60)
	SmpSynch              sv.SmpSynch       // synchronization mode
}

// ChannelSetting describes the electrical model of one 9-2LE channel.
// RMS is specified in secondary amperes/volts, and PhaseDeg is the electrical angle.
type ChannelSetting struct {
	RMS       float64
	PhaseDeg  float64
	Frequency float64
	Quality   sv.Quality
}

// TimingStats describes scheduling quality of the publisher loop.
// A skipped sample is a protocol slot that could not be transmitted before the
// following slot became due. The service advances smpCnt and waveform phase for
// such slots instead of sending a burst of stale frames.
type TimingStats struct {
	FramesSent     uint64
	SkippedSamples uint64
	LateWakeups    uint64
	MaxLateness    time.Duration
	// FramesPerSecond is measured over the last monitoring interval, not on wire.
	FramesPerSecond float64
}

type publisherScheduler interface {
	Now() time.Time
	WaitUntil(context.Context, time.Time) bool
	Stop()
}

const nanosecondsPerSecond = int64(time.Second)

type monotonicScheduler struct {
	timer *time.Timer
}

func newMonotonicScheduler() *monotonicScheduler {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	return &monotonicScheduler{timer: timer}
}

func (*monotonicScheduler) Now() time.Time { return time.Now() }

func (s *monotonicScheduler) Stop() { s.timer.Stop() }

func (s *monotonicScheduler) WaitUntil(ctx context.Context, deadline time.Time) bool {
	delay := time.Until(deadline)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	s.timer.Reset(delay)
	select {
	case <-ctx.Done():
		if !s.timer.Stop() {
			select {
			case <-s.timer.C:
			default:
			}
		}
		return false
	case <-s.timer.C:
		return true
	}
}

// samplesPerSecond returns the total sampling frequency.
func (c *PublisherConfig) samplesPerSecond() int {
	return int(c.SmpRate) * int(c.SampleTimingFrequency)
}

// sampleInterval returns the interval between samples.
func (c *PublisherConfig) sampleInterval() time.Duration {
	sps := c.samplesPerSecond()
	if sps <= 0 {
		return 250 * time.Microsecond // fallback: 4000 sps
	}
	return time.Second / time.Duration(sps)
}

func (c *PublisherConfig) smpCntModulo() uint16 {
	sps := c.samplesPerSecond()
	if sps <= 0 || sps > 0x10000 {
		return 0
	}
	return uint16(sps)
}

// sampleSlotAt returns the absolute SV sample slot containing t. Keeping the
// calculation rational avoids the cumulative drift caused by repeatedly
// adding time.Second / samplesPerSecond (notably at 4,800 samples/s).
func sampleSlotAt(t time.Time, samplesPerSecond int) int64 {
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}
	rate := int64(samplesPerSecond)
	return t.Unix()*rate + int64(t.Nanosecond())*rate/nanosecondsPerSecond
}

// sampleSlotStart returns the first representable nanosecond belonging to an
// absolute sample slot. The ceiling division is important for rates which do
// not divide one second exactly, such as 4,800 samples/s.
func sampleSlotStart(slot int64, samplesPerSecond int) time.Time {
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}
	rate := int64(samplesPerSecond)
	seconds := slot / rate
	remainder := slot % rate
	if remainder < 0 {
		remainder += rate
		seconds--
	}
	nanoseconds := (remainder*nanosecondsPerSecond + rate - 1) / rate
	return time.Unix(seconds, nanoseconds).UTC()
}

// nextSecondSampleSlot returns the zero-count sample slot at the beginning of
// the first whole application-clock second strictly after t. SV publication
// starts from this slot so that the first frame is scheduled with smpCnt=0.
func nextSecondSampleSlot(t time.Time, samplesPerSecond int) int64 {
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}
	return (t.Unix() + 1) * int64(samplesPerSecond)
}

// sourceDurationToSystem converts an interval on the application-clock scale
// to the local monotonic scale used by time.Timer. Offset corrections do not
// affect an interval; the PTP Client frequency correction does.
func sourceDurationToSystem(interval time.Duration, status appclock.SourceStatus) time.Duration {
	if interval <= 0 || !status.PTPEnabled || status.FrequencyPPB == 0 {
		return interval
	}
	rate := 1 + status.FrequencyPPB/1_000_000_000
	if rate <= 0 {
		return interval
	}
	converted := time.Duration(float64(interval) / rate)
	if converted <= 0 {
		return time.Nanosecond
	}
	return converted
}

// systemDurationToSource performs the inverse conversion for comparing timer
// lateness with lateness observed on the application-clock scale.
func systemDurationToSource(interval time.Duration, status appclock.SourceStatus) time.Duration {
	if interval <= 0 || !status.PTPEnabled || status.FrequencyPPB == 0 {
		return interval
	}
	rate := 1 + status.FrequencyPPB/1_000_000_000
	if rate <= 0 {
		return interval
	}
	return time.Duration(float64(interval) * rate)
}

// Service publishes an SV stream (IEC 61850-9-2LE).
//
// It transmits SV frames at a fixed rate of SmpRate × SampleTimingFrequency samples/s.
// smpCnt cycles from 0 to SmpRate*SampleTimingFrequency-1.
type Service struct {
	cfg  PublisherConfig
	sink FrameSink
	log  *slog.Logger

	mu           sync.Mutex
	settings     [sv.NumChannels]ChannelSetting
	ready        chan struct{}
	readyOnce    sync.Once
	sim          bool
	smpSynch     sv.SmpSynch
	smpCnt       uint16
	sample       uint64
	timeSource   appclock.Source
	timing       TimingStats
	transmission *transmissionTracker
}

// New creates an SV publisher. source must be the same application clock
// disciplined by the PTP Client. When PTP is disabled, ApplicationClock
// falls back to system time itself.
func New(cfg PublisherConfig, sink FrameSink, log *slog.Logger, source appclock.Source) *Service {
	if log == nil {
		log = slog.Default()
	}
	if source == nil {
		source = appclock.SystemSource{}
	}
	s := &Service{
		cfg:        cfg,
		sink:       sink,
		log:        log,
		ready:      make(chan struct{}),
		smpSynch:   cfg.SmpSynch,
		timeSource: source,
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		s.settings[ch] = ChannelSetting{
			Frequency: float64(cfg.SampleTimingFrequency),
			Quality:   sv.QualityValid,
		}
	}
	return s
}

// SetWaveform enables sinusoidal sample generation from RMS, phase, and frequency.
func (s *Service) SetWaveform(settings [sv.NumChannels]ChannelSetting, simulation bool, smpSynch sv.SmpSynch) error {
	return s.setWaveform(settings, simulation, smpSynch, nil)
}

func (s *Service) setWaveform(settings [sv.NumChannels]ChannelSetting, simulation bool, smpSynch sv.SmpSynch, transmission *transmissionTracker) error {
	samplesPerSecond := s.cfg.samplesPerSecond()
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}
	nyquist := float64(samplesPerSecond) / 2
	for ch, setting := range settings {
		if !isFinite(setting.Frequency) || setting.Frequency <= 0 {
			return fmt.Errorf("sv_pub: %s frequency must be positive", sv.ChannelNames[ch])
		}
		if setting.Frequency > nyquist {
			return fmt.Errorf("sv_pub: %s frequency %.6g Hz exceeds Nyquist frequency %.6g Hz", sv.ChannelNames[ch], setting.Frequency, nyquist)
		}
		if !isFinite(setting.RMS) || setting.RMS < 0 {
			return fmt.Errorf("sv_pub: %s RMS must be non-negative", sv.ChannelNames[ch])
		}
		if !isFinite(setting.PhaseDeg) {
			return fmt.Errorf("sv_pub: %s phase must be finite", sv.ChannelNames[ch])
		}
		scale := channelScale(ch)
		peakRaw := setting.RMS * math.Sqrt2 * scale
		if !isFinite(peakRaw) || peakRaw > float64(maxInt32) {
			return fmt.Errorf("sv_pub: %s RMS %.6g exceeds raw int32 range", sv.ChannelNames[ch], setting.RMS)
		}
	}
	s.mu.Lock()
	if s.transmission != nil {
		s.transmission.complete(SendReceipt{Err: errors.New("waveform superseded before transmission")})
	}
	s.transmission = transmission
	s.settings = settings
	s.sim = simulation
	s.smpSynch = smpSynch
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
	return nil
}

// SchedulerStats returns a consistent snapshot of publisher timing counters.
func (s *Service) SchedulerStats() TimingStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timing
}

// Run starts the SV publication loop and blocks until the context is canceled.
//
// The first SV frame is scheduled for the start of the next whole application-clock
// second with smpCnt=0. One frame is then sent at each slot boundary, and smpCnt
// wraps at SmpRate*SampleTimingFrequency.
func (s *Service) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-s.ready:
	}

	interval := s.cfg.sampleInterval()
	sps := s.cfg.samplesPerSecond()

	s.log.Debug("sv_pub started",
		"name", s.cfg.Name,
		"dst_mac", s.cfg.DstMAC.String(),
		"app_id", s.cfg.AppID,
		"sv_id", s.cfg.SvID,
		"smp_rate", s.cfg.SmpRate,
		"sample_timing_frequency", s.cfg.SampleTimingFrequency,
		"sps", sps,
		"interval", interval,
	)

	scheduler := newPublisherScheduler()
	defer scheduler.Stop()
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		s.monitorTiming(monitorCtx)
	}()
	defer func() {
		cancelMonitor()
		<-monitorDone
	}()
	if err := s.runScheduled(ctx, scheduler, interval); err != nil {
		return err
	}
	return scheduler.Err()
}

func (s *Service) runScheduled(ctx context.Context, scheduler publisherScheduler, interval time.Duration) error {
	samplesPerSecond := s.cfg.samplesPerSecond()
	if samplesPerSecond <= 0 && interval > 0 {
		samplesPerSecond = int(time.Second / interval)
	}
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}

	clockNow := s.timeSource.Now()
	nextSlot := nextSecondSampleSlot(clockNow, samplesPerSecond)
	nextDelay := sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
	nextDeadline := scheduler.Now().Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
	waitingForSecond := true
	startupBoundaryMisses := 0
	startupStepThreshold := interval
	if startupStepThreshold <= 0 {
		startupStepThreshold = time.Second / time.Duration(samplesPerSecond)
	}

	for {
		if !scheduler.WaitUntil(ctx, nextDeadline) {
			s.log.Debug("sv_pub stopping", "name", s.cfg.Name)
			return nil
		}
		if ctx.Err() != nil {
			s.log.Debug("sv_pub stopping", "name", s.cfg.Name)
			return nil
		}

		// Read the application clock before the monotonic lateness sample. A
		// possible GC/preemption pause is then included in scheduler lateness
		// instead of being mistaken for a forward PTP step.
		clockNow = s.timeSource.Now()
		now := scheduler.Now()
		currentSlot := sampleSlotAt(clockNow, samplesPerSecond)
		if waitingForSecond {
			slotInSecond := currentSlot % int64(samplesPerSecond)
			if slotInSecond < 0 {
				slotInSecond += int64(samplesPerSecond)
			}
			switch {
			case currentSlot < nextSlot:
				// The timer woke early or PTP stepped backwards. A large backward
				// step may make an earlier whole-second boundary reachable again.
				if rebasedSlot := nextSecondSampleSlot(clockNow, samplesPerSecond); rebasedSlot < nextSlot {
					nextSlot = rebasedSlot
				}
				nextDelay = sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
				nextDeadline = now.Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
				continue
			case currentSlot > nextSlot && slotInSecond == 0:
				// A forward step landed in the zero-count slot of a later second.
				// That boundary is suitable for the first frame.
				nextSlot = currentSlot
			case currentSlot > nextSlot:
				targetTime := sampleSlotStart(nextSlot, samplesPerSecond)
				clockLateness := clockNow.Sub(targetTime)
				schedulerLateness := now.Sub(nextDeadline)
				if schedulerLateness < 0 {
					schedulerLateness = 0
				}
				expectedClockLateness := systemDurationToSource(schedulerLateness, appclock.StatusOf(s.timeSource))
				if clockLateness-expectedClockLateness >= startupStepThreshold {
					// Application time moved forward independently of the monotonic
					// timer. Retarget instead of sending a stale zero-count sample.
					nextSlot = nextSecondSampleSlot(clockNow, samplesPerSecond)
					nextDelay = sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
					nextDeadline = now.Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
					continue
				}
				startupBoundaryMisses++
				// A zero-count sample is already stale once its sample slot has
				// elapsed. Keep retrying at whole-second boundaries until one is
				// reached or the publisher is cancelled; never send a stale zero.
				s.log.Warn("sv_pub: whole-second start boundary missed; retrying",
					"name", s.cfg.Name,
					"misses", startupBoundaryMisses,
					"lateness", clockLateness,
				)
				nextSlot = nextSecondSampleSlot(clockNow, samplesPerSecond)
				nextDelay = sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
				nextDeadline = now.Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
				continue
			}
		} else if currentSlot < nextSlot {
			// A PTP step backwards may put the old target far into the future.
			// Rebase to the new clock scale, while a normal early timer wakeup
			// simply waits for the same slot again.
			if nextSlot-currentSlot > 1 {
				nextSlot = currentSlot + 1
			}
			nextDelay = sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
			nextDeadline = now.Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
			continue
		}

		skipped := uint64(currentSlot - nextSlot)
		lateness := clockNow.Sub(sampleSlotStart(nextSlot, samplesPerSecond))
		s.recordWakeup(skipped, lateness)
		s.alignToClockSlot(currentSlot, samplesPerSecond)
		if err := s.transmit(); err != nil {
			return err
		}
		s.recordFrameSent()
		waitingForSecond = false
		nextSlot = currentSlot + 1
		nextDelay = sampleSlotStart(nextSlot, samplesPerSecond).Sub(clockNow)
		nextDeadline = now.Add(sourceDurationToSystem(nextDelay, appclock.StatusOf(s.timeSource)))
	}
}

func (s *Service) alignToClockSlot(slot int64, samplesPerSecond int) {
	s.mu.Lock()
	if slot < 0 {
		s.sample = 0
		s.smpCnt = 0
	} else {
		s.sample = uint64(slot)
		s.smpCnt = uint16(slot % int64(samplesPerSecond))
	}
	s.mu.Unlock()
}

func (s *Service) recordWakeup(skipped uint64, lateness time.Duration) {
	s.mu.Lock()
	if lateness > 0 {
		s.timing.LateWakeups++
		if lateness > s.timing.MaxLateness {
			s.timing.MaxLateness = lateness
		}
	}
	s.timing.SkippedSamples += skipped
	s.mu.Unlock()
}

func (s *Service) recordFrameSent() {
	s.mu.Lock()
	s.timing.FramesSent++
	s.mu.Unlock()
}

// transmit builds and sends one SV frame.
func (s *Service) transmit() error {
	s.mu.Lock()
	channels, quality, err := synthesizeChannels(s.sample, s.cfg.samplesPerSecond(), s.settings)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	simulation := s.sim
	transmission := s.transmission
	s.transmission = nil
	smpSynch := s.smpSynch
	asdu := sv.ASDU{
		DstMAC:     s.cfg.DstMAC,
		SrcMAC:     s.cfg.SrcMAC,
		VLAN:       s.cfg.VLAN,
		AppID:      s.cfg.AppID,
		SvID:       s.cfg.SvID,
		SmpCnt:     s.smpCnt,
		ConfRev:    s.cfg.ConfRev,
		Simulation: simulation,
		SmpSynch:   smpSynch,
		Channels:   channels,
		Quality:    quality,
	}
	s.smpCnt++
	s.sample++
	if modulo := s.cfg.smpCntModulo(); modulo > 0 && s.smpCnt >= modulo {
		s.smpCnt = 0
	}
	s.mu.Unlock()

	frame := sv.Encode([]sv.ASDU{asdu})
	var receipt SendReceipt
	if transmission != nil {
		receipt.AppTime = s.timeSource.Now()
		receipt.At = time.Now()
	}
	if err := s.sink.WriteFrame(frame); err != nil {
		if transmission != nil {
			receipt.Err = err
			transmission.complete(receipt)
		}
		s.log.Warn("sv_pub: write frame failed",
			"name", s.cfg.Name,
			"err", err,
		)
		return fmt.Errorf("sv_pub %s: write frame: %w", s.cfg.Name, err)
	}
	if transmission != nil {
		transmission.complete(receipt)
	}
	return nil
}

const maxInt32 = int64(1<<31 - 1)

func synthesizeChannels(sample uint64, samplesPerSecond int, settings [sv.NumChannels]ChannelSetting) ([sv.NumChannels]int32, [sv.NumChannels]sv.Quality, error) {
	var channels [sv.NumChannels]int32
	var quality [sv.NumChannels]sv.Quality
	if samplesPerSecond <= 0 {
		samplesPerSecond = 4000
	}
	for ch, setting := range settings {
		if !isFinite(setting.RMS) || !isFinite(setting.PhaseDeg) || !isFinite(setting.Frequency) {
			return channels, quality, fmt.Errorf("sv_pub: %s waveform contains a non-finite value", sv.ChannelNames[ch])
		}
		phaseStep := 2 * math.Pi * setting.Frequency / float64(samplesPerSecond)
		phase := math.Remainder(float64(sample)*phaseStep+setting.PhaseDeg*math.Pi/180, 2*math.Pi)
		raw := math.Round(setting.RMS * math.Sqrt2 * math.Sin(phase) * channelScale(ch))
		if !isFinite(raw) || raw < -float64(maxInt32)-1 || raw > float64(maxInt32) {
			return channels, quality, fmt.Errorf("sv_pub: %s synthesized sample exceeds raw int32 range", sv.ChannelNames[ch])
		}
		channels[ch] = int32(raw)
		quality[ch] = setting.Quality
	}
	return channels, quality, nil
}

func channelScale(ch int) float64 {
	if ch >= sv.ChUa {
		return sv.VoltageScale
	}
	return sv.CurrentScale
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
