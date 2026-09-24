package svpub

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/sv"
)

type fixedTimeSource struct {
	now    time.Time
	status appclock.SourceStatus
}

func (s *fixedTimeSource) Now() time.Time                      { return s.now }
func (s *fixedTimeSource) SourceStatus() appclock.SourceStatus { return s.status }

type fakeScheduler struct {
	now        time.Time
	lateness   []time.Duration
	clockSteps []time.Duration
	clock      *fixedTimeSource
	wakeup     int
}

func (s *fakeScheduler) Now() time.Time { return s.now }

func (s *fakeScheduler) WaitUntil(ctx context.Context, deadline time.Time) bool {
	if ctx.Err() != nil || s.wakeup >= len(s.lateness) {
		return false
	}
	next := deadline.Add(s.lateness[s.wakeup])
	if s.clock != nil {
		s.clock.now = s.clock.now.Add(next.Sub(s.now))
		if s.wakeup < len(s.clockSteps) {
			s.clock.now = s.clock.now.Add(s.clockSteps[s.wakeup])
		}
	}
	s.now = next
	s.wakeup++
	return true
}

func (*fakeScheduler) Stop() {}

type blockingScheduler struct {
	now      time.Time
	deadline chan time.Time
}

func (s *blockingScheduler) Now() time.Time { return s.now }

func (s *blockingScheduler) WaitUntil(ctx context.Context, deadline time.Time) bool {
	s.deadline <- deadline
	<-ctx.Done()
	return false
}

func (*blockingScheduler) Stop() {}

// mockSink records transmitted frames.
type mockSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (m *mockSink) WriteFrame(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(frame))
	copy(cp, frame)
	m.frames = append(m.frames, cp)
	return nil
}

func (m *mockSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.frames)
}

func (m *mockSink) getFrame(i int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frames[i]
}

func newTestService(sink *mockSink) *Service {
	cfg := PublisherConfig{
		Name:                  "test_sv",
		DstMAC:                net.HardwareAddr{0x01, 0x0C, 0xCD, 0x04, 0x00, 0x01},
		SrcMAC:                net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:                 0x4001,
		SvID:                  "PBMTMU0101",
		ConfRev:               1,
		VLAN:                  &ethernet.VLANTag{Priority: 4, VID: 1},
		SmpRate:               80,
		SampleTimingFrequency: 50,
		SmpSynch:              sv.SmpSynchNone,
	}
	log := slog.Default()
	return New(cfg, sink, log, appclock.SystemSource{})
}

// TestNewInitializesState verifies the initial state.
func TestNewInitializesState(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)

	if svc.smpCnt != 0 {
		t.Errorf("smpCnt: want 0, got %d", svc.smpCnt)
	}
}

func TestPublisherIsNotReadyUntilWaveformIsConfigured(t *testing.T) {
	svc := newTestService(&mockSink{})
	select {
	case <-svc.ready:
		t.Fatal("new publisher is ready before waveform configuration")
	default:
	}

	var settings [sv.NumChannels]ChannelSetting
	for ch := range settings {
		settings[ch] = ChannelSetting{Frequency: 50, Quality: sv.QualityValid}
	}
	if err := svc.SetWaveform(settings, false, sv.SmpSynchNone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-svc.ready:
	default:
		t.Fatal("publisher did not become ready after waveform configuration")
	}
}

// TestTransmitEncodesDecodableFrame verifies that transmit creates a valid SV frame.
func TestTransmitEncodesDecodableFrame(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)

	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}

	if sink.count() != 1 {
		t.Fatalf("frames: want 1, got %d", sink.count())
	}

	frame := sink.getFrame(0)
	asdus, err := sv.Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if len(asdus) != 1 {
		t.Fatalf("ASDU count: want 1, got %d", len(asdus))
	}

	a := asdus[0]
	if a.SvID != "PBMTMU0101" {
		t.Errorf("SvID: want PBMTMU0101, got %s", a.SvID)
	}
	if a.AppID != 0x4001 {
		t.Errorf("AppID: want 0x4001, got 0x%04X", a.AppID)
	}
	if a.SmpCnt != 0 {
		t.Errorf("SmpCnt: want 0, got %d", a.SmpCnt)
	}
	if a.HasRefrTm || a.DatSet != "" || a.HasSmpRate || a.HasSmpMod {
		t.Fatalf("9-2LE Publisher sent optional fields: refrTm=%t datSet=%q smpRate=%t smpMod=%t", a.HasRefrTm, a.DatSet, a.HasSmpRate, a.HasSmpMod)
	}
	if len(frame) != 126 {
		t.Fatalf("default VLAN 9-2LE frame length: got %d, want 126", len(frame))
	}
	if got := binary.BigEndian.Uint16(frame[12:14]); got != ethernet.EtherTypeVLAN {
		t.Fatalf("default VLAN TPID: got 0x%04X, want 0x%04X", got, ethernet.EtherTypeVLAN)
	}
	if got := binary.BigEndian.Uint16(frame[14:16]); got != 0x8001 {
		t.Fatalf("default VLAN TCI: got 0x%04X, want 0x8001", got)
	}
	if got := binary.BigEndian.Uint16(frame[16:18]); got != sv.EtherType {
		t.Fatalf("SV EtherType: got 0x%04X, want 0x%04X", got, sv.EtherType)
	}
	if got := binary.BigEndian.Uint16(frame[18:20]); got != 0x4001 {
		t.Fatalf("default APPID: got 0x%04X, want 0x4001", got)
	}
	if got := binary.BigEndian.Uint16(frame[20:22]); got != 108 {
		t.Fatalf("minimal 9-2LE APDU Length: got %d, want 108", got)
	}
}

func TestTransmitPreservesSelectedSmpSynch(t *testing.T) {
	statuses := []struct {
		name   string
		status appclock.SourceStatus
	}{
		{name: "PTP disabled"},
		{name: "PTP unlocked", status: appclock.SourceStatus{PTPEnabled: true}},
		{name: "PTP locked", status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true}},
	}
	modes := []sv.SmpSynch{sv.SmpSynchNone, sv.SmpSynchLocal, sv.SmpSynchGlobal}
	for _, status := range statuses {
		for _, mode := range modes {
			t.Run(status.name+"/"+mode.String(), func(t *testing.T) {
				sink := &mockSink{}
				source := &fixedTimeSource{
					now:    time.Date(2026, 8, 5, 12, 30, 15, 125_000_000, time.UTC),
					status: status.status,
				}
				svc := newTestService(sink)
				svc.cfg.DatSet = "PhsMeas1"
				svc.timeSource = source
				svc.smpSynch = mode
				if err := svc.transmit(); err != nil {
					t.Fatal(err)
				}
				asdus, err := sv.Decode(sink.getFrame(0))
				if err != nil {
					t.Fatal(err)
				}
				asdu := asdus[0]
				if asdu.HasRefrTm || asdu.DatSet != "" || asdu.HasSmpRate || asdu.HasSmpMod {
					t.Fatalf("9-2LE Publisher sent optional fields: refrTm=%t datSet=%q smpRate=%t smpMod=%t", asdu.HasRefrTm, asdu.DatSet, asdu.HasSmpRate, asdu.HasSmpMod)
				}
				if asdu.SmpSynch != mode {
					t.Fatalf("smpSynch: want selected %s, got %s", mode, asdu.SmpSynch)
				}
			})
		}
	}
}

// TestTransmitIncrementsSmpCnt verifies that smpCnt is incremented.
func TestTransmitIncrementsSmpCnt(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)

	for i := 0; i < 5; i++ {
		if err := svc.transmit(); err != nil {
			t.Fatal(err)
		}
	}

	if sink.count() != 5 {
		t.Fatalf("frames: want 5, got %d", sink.count())
	}

	for i := 0; i < 5; i++ {
		asdus, err := sv.Decode(sink.getFrame(i))
		if err != nil {
			t.Fatalf("Decode frame %d: %v", i, err)
		}
		if asdus[0].SmpCnt != uint16(i) {
			t.Errorf("frame %d SmpCnt: want %d, got %d", i, i, asdus[0].SmpCnt)
		}
	}
}

// TestSmpCntWraps verifies that smpCnt wraps at SmpRate*SampleTimingFrequency.
func TestSmpCntWraps(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	modulo := svc.cfg.samplesPerSecond()

	// Transmit SmpRate*SampleTimingFrequency frames; smpCnt must wrap to 0.
	for i := 0; i < modulo; i++ {
		if err := svc.transmit(); err != nil {
			t.Fatal(err)
		}
	}

	// The next frame must have smpCnt=0.
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}

	asdus, err := sv.Decode(sink.getFrame(modulo))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if asdus[0].SmpCnt != 0 {
		t.Errorf("SmpCnt after wrap: want 0, got %d", asdus[0].SmpCnt)
	}
}

func TestSetWaveformGeneratesInstantaneousSamples(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)

	var settings [sv.NumChannels]ChannelSetting
	for ch := 0; ch < sv.NumChannels; ch++ {
		settings[ch] = ChannelSetting{Frequency: 50, Quality: sv.QualityValid}
	}
	settings[sv.ChIa] = ChannelSetting{RMS: 1, PhaseDeg: 90, Frequency: 50, Quality: sv.QualityTest}
	settings[sv.ChUa] = ChannelSetting{RMS: 100, PhaseDeg: 90, Frequency: 50, Quality: sv.QualityValid}

	if err := svc.SetWaveform(settings, true, sv.SmpSynchGlobal); err != nil {
		t.Fatal(err)
	}
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}

	asdus, err := sv.Decode(sink.getFrame(0))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	wantIa := int32(math.Round(math.Sqrt2 * sv.CurrentScale))
	if asdus[0].Channels[sv.ChIa] != wantIa {
		t.Errorf("Ia raw: want %d, got %d", wantIa, asdus[0].Channels[sv.ChIa])
	}
	wantUa := int32(math.Round(100 * math.Sqrt2 * sv.VoltageScale))
	if asdus[0].Channels[sv.ChUa] != wantUa {
		t.Errorf("Ua raw: want %d, got %d", wantUa, asdus[0].Channels[sv.ChUa])
	}
	if asdus[0].Quality[sv.ChIa] != sv.QualityTest {
		t.Errorf("Ia quality: want %v, got %v", sv.QualityTest, asdus[0].Quality[sv.ChIa])
	}
	if !asdus[0].Simulation {
		t.Error("Simulation: want true")
	}
	if asdus[0].SmpSynch != sv.SmpSynchGlobal {
		t.Errorf("SmpSynch: want global, got %s", asdus[0].SmpSynch.String())
	}
}

func TestSetWaveformRejectsInvalidValues(t *testing.T) {
	valid := [sv.NumChannels]ChannelSetting{}
	for ch := range valid {
		valid[ch] = ChannelSetting{RMS: 1, Frequency: 50, Quality: sv.QualityValid}
	}

	tests := []struct {
		name   string
		change func(*ChannelSetting)
	}{
		{name: "NaN RMS", change: func(s *ChannelSetting) { s.RMS = math.NaN() }},
		{name: "infinite RMS", change: func(s *ChannelSetting) { s.RMS = math.Inf(1) }},
		{name: "negative RMS", change: func(s *ChannelSetting) { s.RMS = -1 }},
		{name: "raw overflow", change: func(s *ChannelSetting) { s.RMS = float64(maxInt32) }},
		{name: "NaN phase", change: func(s *ChannelSetting) { s.PhaseDeg = math.NaN() }},
		{name: "infinite frequency", change: func(s *ChannelSetting) { s.Frequency = math.Inf(1) }},
		{name: "zero frequency", change: func(s *ChannelSetting) { s.Frequency = 0 }},
		{name: "above Nyquist", change: func(s *ChannelSetting) { s.Frequency = 2001 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := valid
			tt.change(&settings[sv.ChIa])
			if err := newTestService(&mockSink{}).SetWaveform(settings, false, sv.SmpSynchNone); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSynthesizeChannelsRejectsNonFiniteAndOverflow(t *testing.T) {
	settings := [sv.NumChannels]ChannelSetting{}
	for ch := range settings {
		settings[ch] = ChannelSetting{RMS: 1, Frequency: 50}
	}
	settings[sv.ChIa].RMS = math.NaN()
	if _, _, err := synthesizeChannels(0, 4000, settings); err == nil {
		t.Fatal("expected non-finite value error")
	}
	settings[sv.ChIa].RMS = float64(maxInt32)
	settings[sv.ChIa].PhaseDeg = 90
	if _, _, err := synthesizeChannels(0, 4000, settings); err == nil {
		t.Fatal("expected raw overflow error")
	}
}

func TestRunScheduledSkipsLateSlotsWithoutBurst(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	interval := svc.cfg.sampleInterval()
	clock := &fixedTimeSource{
		now:    time.Unix(0, 0).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &fakeScheduler{
		now:      clock.now,
		clock:    clock,
		lateness: []time.Duration{0, 2*interval + 10*time.Microsecond, 0},
	}
	if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 3 {
		t.Fatalf("frames: want one per wakeup (3), got %d", sink.count())
	}
	wantSmpCnt := []uint16{0, 3, 4}
	for i, want := range wantSmpCnt {
		asdus, err := sv.Decode(sink.getFrame(i))
		if err != nil {
			t.Fatal(err)
		}
		if got := asdus[0].SmpCnt; got != want {
			t.Fatalf("frame %d smpCnt: want %d, got %d", i, want, got)
		}
	}
	stats := svc.SchedulerStats()
	if stats.FramesSent != 3 || stats.SkippedSamples != 2 || stats.LateWakeups != 1 {
		t.Fatalf("unexpected scheduler stats: %+v", stats)
	}
	if want := 2*interval + 10*time.Microsecond; stats.MaxLateness != want {
		t.Fatalf("max lateness: want %v, got %v", want, stats.MaxLateness)
	}
}

func TestRunScheduledStartsOnNextApplicationClockSecond(t *testing.T) {
	tests := []struct {
		name                  string
		start                 time.Time
		sampleTimingFrequency uint16
	}{
		{name: "4000 sps mid-second", start: time.Unix(100, 125_000_000).UTC(), sampleTimingFrequency: 50},
		{name: "4800 sps mid-second", start: time.Unix(100, 333_333_333).UTC(), sampleTimingFrequency: 60},
		{name: "already on boundary", start: time.Unix(100, 0).UTC(), sampleTimingFrequency: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &mockSink{}
			svc := newTestService(sink)
			svc.cfg.SampleTimingFrequency = tt.sampleTimingFrequency
			interval := svc.cfg.sampleInterval()
			clock := &fixedTimeSource{
				now:    tt.start,
				status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
			}
			svc.timeSource = clock
			scheduler := &fakeScheduler{now: clock.now, clock: clock, lateness: []time.Duration{0}}

			if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
				t.Fatal(err)
			}
			if sink.count() != 1 {
				t.Fatalf("frames: want 1, got %d", sink.count())
			}
			wantTime := time.Unix(tt.start.Unix()+1, 0).UTC()
			if !clock.now.Equal(wantTime) {
				t.Fatalf("first frame time: want %s, got %s", wantTime, clock.now)
			}
			asdus, err := sv.Decode(sink.getFrame(0))
			if err != nil {
				t.Fatal(err)
			}
			if got := asdus[0].SmpCnt; got != 0 {
				t.Fatalf("first smpCnt: want 0, got %d", got)
			}
			stats := svc.SchedulerStats()
			if stats.FramesSent != 1 || stats.SkippedSamples != 0 {
				t.Fatalf("unexpected scheduler stats: %+v", stats)
			}
		})
	}
}

func TestRunScheduledResetsCounterOnApplicationClockSecond(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	interval := svc.cfg.sampleInterval()
	clock := &fixedTimeSource{
		now:    time.Unix(100, 999_900_000).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &fakeScheduler{now: clock.now, clock: clock, lateness: []time.Duration{0}}

	if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
		t.Fatal(err)
	}
	asdus, err := sv.Decode(sink.getFrame(0))
	if err != nil {
		t.Fatal(err)
	}
	if got := asdus[0].SmpCnt; got != 0 {
		t.Fatalf("smpCnt at PTP second boundary: want 0, got %d", got)
	}
}

func TestRunScheduledTracksForwardPTPStep(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	interval := svc.cfg.sampleInterval()
	clock := &fixedTimeSource{
		now:    time.Unix(100, 125_000_000).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &fakeScheduler{
		now:        clock.now,
		clock:      clock,
		lateness:   []time.Duration{0, 0},
		clockSteps: []time.Duration{0, 2 * interval},
	}

	if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 2 {
		t.Fatalf("frames: want 2, got %d", sink.count())
	}
	for i, want := range []uint16{0, 3} {
		asdus, err := sv.Decode(sink.getFrame(i))
		if err != nil {
			t.Fatal(err)
		}
		if got := asdus[0].SmpCnt; got != want {
			t.Fatalf("frame %d smpCnt after PTP step: want %d, got %d", i, want, got)
		}
	}
	if got := svc.SchedulerStats().SkippedSamples; got != 2 {
		t.Fatalf("skipped samples after PTP step: want 2, got %d", got)
	}
}

func TestRunScheduledRebasesInitialSecondAfterPTPStep(t *testing.T) {
	tests := []struct {
		name     string
		step     time.Duration
		wantTime time.Time
	}{
		{name: "forward", step: 500 * time.Millisecond, wantTime: time.Unix(102, 0).UTC()},
		{name: "backward", step: -500 * time.Millisecond, wantTime: time.Unix(101, 0).UTC()},
		{name: "backward across seconds", step: -10_500 * time.Millisecond, wantTime: time.Unix(91, 0).UTC()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &mockSink{}
			svc := newTestService(sink)
			interval := svc.cfg.sampleInterval()
			clock := &fixedTimeSource{
				now:    time.Unix(100, 125_000_000).UTC(),
				status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
			}
			svc.timeSource = clock
			scheduler := &fakeScheduler{
				now:        clock.now,
				clock:      clock,
				lateness:   []time.Duration{0, 0},
				clockSteps: []time.Duration{tt.step},
			}

			if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
				t.Fatal(err)
			}
			if scheduler.wakeup != 2 {
				t.Fatalf("wakeups: want 2, got %d", scheduler.wakeup)
			}
			if !clock.now.Equal(tt.wantTime) {
				t.Fatalf("first frame time: want %s, got %s", tt.wantTime, clock.now)
			}
			if sink.count() != 1 {
				t.Fatalf("frames: want 1, got %d", sink.count())
			}
			asdus, err := sv.Decode(sink.getFrame(0))
			if err != nil {
				t.Fatal(err)
			}
			if got := asdus[0].SmpCnt; got != 0 {
				t.Fatalf("first smpCnt: want 0, got %d", got)
			}
			if stats := svc.SchedulerStats(); stats.FramesSent != 1 || stats.SkippedSamples != 0 {
				t.Fatalf("unexpected scheduler stats: %+v", stats)
			}
		})
	}
}

func TestRunScheduledWaitsForAnotherSecondWhenInitialSlotIsMissed(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	interval := svc.cfg.sampleInterval()
	clock := &fixedTimeSource{
		now:    time.Unix(100, 125_000_000).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &fakeScheduler{
		now:      clock.now,
		clock:    clock,
		lateness: []time.Duration{interval + 10*time.Microsecond, 0},
	}

	if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
		t.Fatal(err)
	}
	wantTime := time.Unix(102, 0).UTC()
	if !clock.now.Equal(wantTime) {
		t.Fatalf("first frame time: want %s, got %s", wantTime, clock.now)
	}
	if sink.count() != 1 {
		t.Fatalf("frames: want 1, got %d", sink.count())
	}
	asdus, err := sv.Decode(sink.getFrame(0))
	if err != nil {
		t.Fatal(err)
	}
	if got := asdus[0].SmpCnt; got != 0 {
		t.Fatalf("first smpCnt: want 0, got %d", got)
	}
	if stats := svc.SchedulerStats(); stats.FramesSent != 1 || stats.SkippedSamples != 0 {
		t.Fatalf("unexpected scheduler stats: %+v", stats)
	}
}

func TestRunScheduledKeepsWaitingAfterRepeatedMissedInitialSlots(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	interval := svc.cfg.sampleInterval()
	clock := &fixedTimeSource{
		now:    time.Unix(100, 125_000_000).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &fakeScheduler{
		now:      clock.now,
		clock:    clock,
		lateness: []time.Duration{interval + 10*time.Microsecond, interval + 10*time.Microsecond, interval + 10*time.Microsecond, 0},
	}

	if err := svc.runScheduled(context.Background(), scheduler, interval); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Fatalf("frames: want 1, got %d", sink.count())
	}
	if scheduler.wakeup != 4 {
		t.Fatalf("wakeups: want 4, got %d", scheduler.wakeup)
	}
	asdus, err := sv.Decode(sink.getFrame(0))
	if err != nil {
		t.Fatal(err)
	}
	if got := asdus[0].SmpCnt; got != 0 {
		t.Fatalf("first smpCnt: want 0, got %d", got)
	}
}

func TestRunScheduledCanBeCancelledWhileWaitingForInitialSecond(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	clock := &fixedTimeSource{
		now:    time.Unix(100, 125_000_000).UTC(),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	svc.timeSource = clock
	scheduler := &blockingScheduler{now: clock.now, deadline: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.runScheduled(ctx, scheduler, svc.cfg.sampleInterval())
	}()

	select {
	case deadline := <-scheduler.deadline:
		if want := time.Unix(101, 0).UTC(); !deadline.Equal(want) {
			t.Fatalf("initial deadline: want %s, got %s", want, deadline)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not receive initial deadline")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runScheduled did not stop after cancellation")
	}
	if sink.count() != 0 {
		t.Fatalf("frames after cancellation: want 0, got %d", sink.count())
	}
	if stats := svc.SchedulerStats(); stats != (TimingStats{}) {
		t.Fatalf("scheduler stats after cancellation: want zero, got %+v", stats)
	}
}

func TestSampleSlotBoundariesDoNotDriftAt4800SPS(t *testing.T) {
	const samplesPerSecond = 4800
	base := time.Unix(1_750_000_000, 0).UTC()
	baseSlot := sampleSlotAt(base, samplesPerSecond)
	for _, offset := range []int64{0, 1, 2, 4799, 4800, 9600} {
		slot := baseSlot + offset
		start := sampleSlotStart(slot, samplesPerSecond)
		if got := sampleSlotAt(start, samplesPerSecond); got != slot {
			t.Fatalf("slot %d start maps to %d", slot, got)
		}
		if offset > 0 {
			if got := sampleSlotAt(start.Add(-time.Nanosecond), samplesPerSecond); got != slot-1 {
				t.Fatalf("nanosecond before slot %d maps to %d, want %d", slot, got, slot-1)
			}
		}
	}
	if got := sampleSlotStart(baseSlot+samplesPerSecond, samplesPerSecond); !got.Equal(base.Add(time.Second)) {
		t.Fatalf("one-second boundary drifted: want %s, got %s", base.Add(time.Second), got)
	}
}

// TestSampleInterval verifies interval calculation.
func TestSampleInterval(t *testing.T) {
	tests := []struct {
		smpRate               uint16
		sampleTimingFrequency uint16
		want                  time.Duration
	}{
		{80, 50, 250 * time.Microsecond},   // 4000 sps
		{256, 50, 78125 * time.Nanosecond}, // 12800 sps
		{80, 60, time.Second / 4800},       // 4800 sps
	}

	for _, tt := range tests {
		cfg := PublisherConfig{SmpRate: tt.smpRate, SampleTimingFrequency: tt.sampleTimingFrequency}
		got := cfg.sampleInterval()
		if got != tt.want {
			t.Errorf("sampleInterval(%d, %dHz): want %v, got %v", tt.smpRate, tt.sampleTimingFrequency, tt.want, got)
		}
	}
}
