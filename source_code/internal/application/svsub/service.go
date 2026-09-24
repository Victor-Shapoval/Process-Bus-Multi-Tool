// Package svsub provides the SV stream reception service (IEC 61850-9-2LE).
// It decodes Ethernet frames, matches subscriptions, tracks smpCnt continuity,
// and publishes diagnostic events.
package svsub

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/sv"
)

// Timeouts and intervals.
const (
	streamLostTimeout = 100 * time.Millisecond // no frames received: stream_lost
	watchdogTick      = 50 * time.Millisecond  // stale-stream check interval
	statsInterval     = 10 * time.Second       // statistics publication interval
	snapshotInterval  = 100 * time.Millisecond // live GUI snapshot update interval

	frequencyCrossingWindow    = 32   // number of zero crossings retained for frequency estimation
	frequencyFilterAlpha       = 0.97 // smoothing factor for the displayed frequency
	minFrequencyHz             = 40.0
	maxFrequencyHz             = 70.0
	phasorWindowPeriods        = 32
	rmsPeriodWindow            = 8
	angleSamplePeriodTolerance = 0.20
	angleCrossingWindow        = 16
	angleBadLimit              = 10
	angleGoodLimit             = 3
)

// Service receives, decodes, and filters SV frames while tracking smpCnt.
type Service struct {
	source ethernet.Source
	subs   []*sv.Subscription
	sink   EventSink
	log    *slog.Logger

	mu      sync.Mutex
	streams map[streamKey]*streamState
}

type streamKey struct {
	dstMAC string
	appID  uint16
	svID   string
}

type streamState struct {
	sub         *sv.Subscription
	lastSmpCnt  uint16
	lastConfRev uint32
	lastSynch   sv.SmpSynch
	lastSeenAt  time.Time
	lastASDU    *sv.ASDU
	seen        bool
	lost        bool

	// stats
	samplesReceived uint64
	gapEvents       uint64
	missedTotal     uint64
	lastStatsAt     time.Time
	lastSnapshotAt  time.Time
	sampleIndex     uint64
	freq            [sv.NumChannels]frequencyEstimator
	window          sampleWindow
	angleStability  [sv.NumChannels]angleStability
	latestStats     *StreamStats
	measuredAt      time.Time
	rmsReady        bool
}

type frequencyEstimator struct {
	lastValue   float64
	lastTime    time.Time
	lastSample  uint64
	crossings   []time.Time
	samples     []float64
	valueHz     float64
	initialized bool
}

type phasorSnapshot struct {
	rms   [sv.NumChannels]float64
	angle [sv.NumChannels]float64
}

type angleStability struct {
	badCount  int
	goodCount int
	unstable  bool
	lastAngle float64
	hasAngle  bool
}

type sampleWindow struct {
	values     [sv.NumChannels][]float64
	index      []uint64
	sumSquares [sv.NumChannels]float64
	pos        int
	count      int
	size       int
}

func New(source ethernet.Source, subs []*sv.Subscription, sink EventSink, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if sink == nil {
		sink = noopSink{}
	}
	return &Service{
		source:  source,
		subs:    subs,
		sink:    sink,
		log:     log,
		streams: make(map[streamKey]*streamState),
	}
}

func (s *Service) Run(ctx context.Context) error {
	if len(s.subs) == 0 {
		return errors.New("svsub: no subscriptions configured")
	}
	s.log.Debug("svsub started", "subscriptions", len(s.subs))
	defer s.log.Debug("svsub stopped")

	wgCtx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watchdog(wgCtx)
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	frames := s.source.Frames()
	errs := s.source.Errors()
	var captureErr error

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			s.log.Warn("capture error", "err", err)
			captureErr = err
		case frame, ok := <-frames:
			if !ok {
				if captureErr != nil {
					return captureErr
				}
				select {
				case err, ok := <-errs:
					if ok && err != nil {
						return err
					}
				default:
				}
				return nil
			}
			s.process(frame)
		}
	}
}

func (s *Service) process(frame ethernet.Frame) {
	asdus, err := sv.Decode(frame.Data)
	if err != nil {
		return
	}
	for i := range asdus {
		a := &asdus[i]
		for _, sub := range s.subs {
			if !sub.Matches(a) {
				continue
			}
			s.handleMatch(sub, a, frame)
			break // first match wins
		}
	}
}

func (s *Service) handleMatch(sub *sv.Subscription, asdu *sv.ASDU, frame ethernet.Frame) {
	receivedAt := frame.Timestamp
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	key := streamKey{
		dstMAC: asdu.DstMAC.String(),
		appID:  asdu.AppID,
		svID:   asdu.SvID,
	}

	s.mu.Lock()
	state, ok := s.streams[key]
	if !ok {
		state = &streamState{sub: sub, lastStatsAt: receivedAt}
		s.streams[key] = state
	}

	isFirst := !state.seen
	wasLost := state.lost
	prevSmpCnt := state.lastSmpCnt
	prevConfRev := state.lastConfRev
	prevSynch := state.lastSynch
	confRevChanged := !isFirst && asdu.ConfRev != prevConfRev

	// Classify continuity before mutating the accepted stream state. A duplicate
	// or reordered frame must not pollute RMS/phasor windows or move the counter
	// baseline backwards. After a confirmed loss or configuration revision,
	// accept the new sample as a fresh baseline because publishers may restart
	// their counter.
	expected, missed, forward := smpCntGap(prevSmpCnt, asdu.SmpCnt, effectiveSmpRate(asdu, sub), effectiveSampleTimingFrequency(sub))
	if !isFirst && !wasLost && !confRevChanged && asdu.SmpCnt != expected && !forward {
		s.mu.Unlock()
		s.log.Debug("svsub ignored duplicate or reordered sample",
			"subscription", sub.Name,
			"previous", prevSmpCnt,
			"actual", asdu.SmpCnt,
		)
		return
	}

	hasGap := !isFirst && !wasLost && !confRevChanged && asdu.SmpCnt != expected && forward && missed > 0
	if wasLost || confRevChanged || hasGap {
		state.resetMeasurements()
	}
	if hasGap {
		state.gapEvents++
		state.missedTotal += uint64(missed)
	}

	state.lastSmpCnt = asdu.SmpCnt
	state.lastConfRev = asdu.ConfRev
	state.lastSynch = asdu.SmpSynch
	state.lastSeenAt = receivedAt
	state.lastASDU = asdu
	state.seen = true
	state.lost = false
	state.samplesReceived++
	state.sampleIndex++
	smpRate := effectiveSmpRate(asdu, sub)
	state.window.add(asdu, smpRate, state.sampleIndex)
	for ch := 0; ch < sv.NumChannels; ch++ {
		state.freq[ch].update(receivedAt, asdu.PhysicalValue(ch), state.sampleIndex)
	}
	s.mu.Unlock()

	// stream_restored
	if wasLost {
		s.sink.OnSVEvent(Event{
			Reason:       ReasonStreamRestored,
			Subscription: sub.Name,
			ASDU:         asdu,
			ReceivedAt:   receivedAt,
		})
	}

	// first_seen
	if isFirst {
		s.sink.OnSVEvent(Event{
			Reason:       ReasonFirstSeen,
			Subscription: sub.Name,
			ASDU:         asdu,
			ReceivedAt:   receivedAt,
		})
		return
	}

	if hasGap {
		s.sink.OnSVEvent(Event{
			Reason:         ReasonSmpCntGap,
			Subscription:   sub.Name,
			ASDU:           asdu,
			ReceivedAt:     receivedAt,
			ExpectedSmpCnt: expected,
			ActualSmpCnt:   asdu.SmpCnt,
			MissedSamples:  missed,
		})
	}

	// confRev changed
	if confRevChanged {
		s.sink.OnSVEvent(Event{
			Reason:       ReasonConfRevChanged,
			Subscription: sub.Name,
			ASDU:         asdu,
			ReceivedAt:   receivedAt,
			PrevConfRev:  prevConfRev,
		})
	}

	// sync changed
	if asdu.SmpSynch != prevSynch {
		s.sink.OnSVEvent(Event{
			Reason:       ReasonSyncChanged,
			Subscription: sub.Name,
			ASDU:         asdu,
			ReceivedAt:   receivedAt,
			PrevSmpSynch: prevSynch,
		})
	}
}

func smpCntGap(previous, current, smpRate, timingFrequency uint16) (expected uint16, missed int, forward bool) {
	expected = previous + 1
	if current == expected {
		return expected, 0, true
	}
	if current == previous {
		return expected, 0, false
	}
	if current > previous {
		return expected, int(current) - int(previous) - 1, true
	}

	modulo := smpCntModulo(smpRate, timingFrequency, previous)
	if modulo == 0 {
		modulo = 1 << 16
	}
	missed = modulo - int(previous) - 1 + int(current)
	if missed < 0 || missed >= modulo/2 {
		return expected, 0, false
	}
	return expected, missed, true
}

func smpCntModulo(smpRate, timingFrequency, previous uint16) int {
	if smpRate == 0 {
		return 0
	}
	if timingFrequency == 50 || timingFrequency == 60 {
		candidate := int(smpRate) * int(timingFrequency)
		if int(previous) >= candidate/2 && int(previous) < candidate {
			return candidate
		}
		return 0
	}
	best := 0
	for _, frequency := range [...]int{50, 60} {
		candidate := int(smpRate) * frequency
		if candidate <= int(previous) || int(previous) < candidate/2 {
			continue
		}
		if best == 0 || candidate < best {
			best = candidate
		}
	}
	return best
}

func effectiveSampleTimingFrequency(sub *sv.Subscription) uint16 {
	if sub != nil {
		return sub.SampleTimingFrequency
	}
	return 0
}

func effectiveSmpRate(asdu *sv.ASDU, sub *sv.Subscription) uint16 {
	if asdu != nil && asdu.SmpRate != 0 {
		return asdu.SmpRate
	}
	if sub != nil && sub.SmpRate != 0 {
		return sub.SmpRate
	}
	return 80
}

func baseVectorIndex(sub *sv.Subscription) int {
	if sub == nil {
		return sv.ChUa
	}
	for i, ch := range sv.ChannelNames {
		if ch == sub.BaseVector {
			return i
		}
	}
	return sv.ChUa
}

func (f *frequencyEstimator) update(now time.Time, value float64, sampleIndex uint64) {
	if !f.initialized {
		f.lastValue = value
		f.lastTime = now
		f.lastSample = sampleIndex
		f.initialized = true
		return
	}
	prevValue := f.lastValue
	prevTime := f.lastTime
	prevSample := f.lastSample
	f.lastValue = value
	f.lastTime = now
	f.lastSample = sampleIndex

	if prevValue >= 0 || value < 0 {
		return
	}
	den := value - prevValue
	if den == 0 {
		return
	}
	fraction := -prevValue / den
	if fraction < 0 || fraction > 1 {
		return
	}
	crossing := prevTime.Add(time.Duration(float64(now.Sub(prevTime)) * fraction))
	sampleCrossing := float64(prevSample) + (float64(sampleIndex)-float64(prevSample))*fraction
	f.addCrossing(crossing, sampleCrossing)
	if len(f.crossings) < 3 {
		return
	}
	estimate := f.windowFrequency()
	if f.valueHz == 0 {
		f.valueHz = estimate
		return
	}
	f.valueHz = frequencyFilterAlpha*f.valueHz + (1-frequencyFilterAlpha)*estimate
}

func (f *frequencyEstimator) addCrossing(crossing time.Time, sampleCrossing float64) {
	if len(f.crossings) > 0 {
		last := f.crossings[len(f.crossings)-1]
		period := crossing.Sub(last).Seconds()
		if period <= 0 {
			return
		}
		freq := 1 / period
		if freq < minFrequencyHz || freq > maxFrequencyHz {
			f.crossings = f.crossings[:0]
			f.samples = f.samples[:0]
		}
	}
	f.crossings = append(f.crossings, crossing)
	f.samples = append(f.samples, sampleCrossing)
	if len(f.crossings) > frequencyCrossingWindow {
		copy(f.crossings, f.crossings[len(f.crossings)-frequencyCrossingWindow:])
		f.crossings = f.crossings[:frequencyCrossingWindow]
		copy(f.samples, f.samples[len(f.samples)-frequencyCrossingWindow:])
		f.samples = f.samples[:frequencyCrossingWindow]
	}
}

func (f *frequencyEstimator) windowFrequency() float64 {
	first := f.crossings[0]
	last := f.crossings[len(f.crossings)-1]
	periods := len(f.crossings) - 1
	elapsed := last.Sub(first).Seconds()
	if periods <= 0 || elapsed <= 0 {
		return f.valueHz
	}
	estimate := float64(periods) / elapsed
	if estimate < minFrequencyHz || estimate > maxFrequencyHz {
		return f.valueHz
	}
	return estimate
}

func (f *frequencyEstimator) lastCrossing() (time.Time, bool) {
	if len(f.crossings) == 0 {
		return time.Time{}, false
	}
	return f.crossings[len(f.crossings)-1], true
}

func (f *frequencyEstimator) samplePeriod() (float64, bool) {
	if len(f.samples) < 2 {
		return 0, false
	}
	first := f.samples[0]
	last := f.samples[len(f.samples)-1]
	periods := len(f.samples) - 1
	if periods <= 0 || last <= first {
		return 0, false
	}
	return (last - first) / float64(periods), true
}

func (f *frequencyEstimator) recentSampleCrossings(limit int) []float64 {
	if limit <= 0 || len(f.samples) == 0 {
		return nil
	}
	start := 0
	if len(f.samples) > limit {
		start = len(f.samples) - limit
	}
	return f.samples[start:]
}

func (w *sampleWindow) add(asdu *sv.ASDU, smpRate uint16, sampleIndex uint64) {
	size := int(smpRate) * phasorWindowPeriods
	if size <= 0 {
		size = 80 * phasorWindowPeriods
	}
	if size > 8192 {
		size = 8192
	}
	if w.size != size {
		w.size = size
		w.pos = 0
		w.count = 0
		w.index = make([]uint64, size)
		w.sumSquares = [sv.NumChannels]float64{}
		for ch := 0; ch < sv.NumChannels; ch++ {
			w.values[ch] = make([]float64, size)
		}
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		if w.count == w.size {
			old := w.values[ch][w.pos]
			w.sumSquares[ch] -= old * old
		}
		value := asdu.PhysicalValue(ch)
		w.values[ch][w.pos] = value
		w.sumSquares[ch] += value * value
	}
	w.index[w.pos] = sampleIndex
	w.pos = (w.pos + 1) % w.size
	if w.count < w.size {
		w.count++
	}
}

func (w *sampleWindow) reset() {
	w.pos = 0
	w.count = 0
	w.sumSquares = [sv.NumChannels]float64{}
}

func (st *streamState) resetMeasurements() {
	st.latestStats = nil
	st.measuredAt = time.Time{}
	st.rmsReady = false
	st.window.reset()
	st.freq = [sv.NumChannels]frequencyEstimator{}
	st.angleStability = [sv.NumChannels]angleStability{}
}

func (w *sampleWindow) oldestPosition() int {
	if w.count == w.size {
		return w.pos
	}
	return 0
}

func (w *sampleWindow) rms() [sv.NumChannels]float64 {
	var out [sv.NumChannels]float64
	if w.count == 0 {
		return out
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		out[ch] = math.Sqrt(math.Max(0, w.sumSquares[ch]) / float64(w.count))
	}
	return out
}

func (w *sampleWindow) rmsBetween(startSample, endSample float64) ([sv.NumChannels]float64, bool) {
	var out [sv.NumChannels]float64
	if w.count == 0 || endSample <= startSample {
		return out, false
	}
	start := w.oldestPosition()
	var count int
	for i := 0; i < w.count; i++ {
		slot := (start + i) % w.size
		sampleIndex := w.index[slot]
		pos := float64(sampleIndex)
		if pos <= startSample || pos > endSample {
			continue
		}
		count++
		for ch := 0; ch < sv.NumChannels; ch++ {
			v := w.values[ch][slot]
			out[ch] += v * v
		}
	}
	if count == 0 {
		return out, false
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		out[ch] = math.Sqrt(out[ch] / float64(count))
	}
	return out, true
}

func (w *sampleWindow) rmsByPeriods(baseCrossings []float64) ([sv.NumChannels]float64, bool) {
	var out [sv.NumChannels]float64
	if len(baseCrossings) < 2 || w.count == 0 {
		return out, false
	}
	oldest := w.oldestPosition()
	newest := (oldest + w.count - 1) % w.size
	minSample := float64(w.index[oldest])
	maxSample := float64(w.index[newest])

	first := 0
	for first < len(baseCrossings) && baseCrossings[first] < minSample {
		first++
	}
	last := len(baseCrossings) - 1
	for last >= first && baseCrossings[last] > maxSample {
		last--
	}
	if last-first+1 < 2 {
		return out, false
	}
	if last-first+1 > rmsPeriodWindow+1 {
		first = last - rmsPeriodWindow
	}
	return w.rmsBetween(baseCrossings[first], baseCrossings[last])
}

func (st *streamState) measurements() phasorSnapshot {
	baseIndex := baseVectorIndex(st.sub)
	if baseIndex < 0 || baseIndex >= sv.NumChannels {
		baseIndex = sv.ChUa
	}
	rms := st.window.rms()
	if periodRMS, ok := st.window.rmsByPeriods(st.freq[baseIndex].samples); ok {
		rms = periodRMS
	}
	out := phasorSnapshot{
		rms: rms,
	}

	_, ok := st.freq[baseIndex].lastCrossing()
	baseSamplePeriod, periodOK := st.freq[baseIndex].samplePeriod()
	baseFrequency := st.freq[baseIndex].valueHz
	if !ok || !periodOK || baseFrequency <= 0 {
		for ch := 0; ch < sv.NumChannels; ch++ {
			out.angle[ch] = math.NaN()
		}
		return out
	}

	for ch := 0; ch < sv.NumChannels; ch++ {
		chSamplePeriod, chPeriodOK := st.freq[ch].samplePeriod()
		angle, ok := st.averageRelativeAngle(ch, baseIndex, baseSamplePeriod)
		stable := ok && chPeriodOK && math.Abs(chSamplePeriod-baseSamplePeriod) <= angleSamplePeriodTolerance
		if stable {
			out.angle[ch] = st.angleStability[ch].accept(angle)
			continue
		}
		if last, ok := st.angleStability[ch].reject(); ok {
			out.angle[ch] = last
		} else {
			out.angle[ch] = math.NaN()
		}
	}
	out.angle[baseIndex] = 0
	return out
}

func (st *streamState) averageRelativeAngle(ch, baseIndex int, baseSamplePeriod float64) (float64, bool) {
	baseCrossings := st.freq[baseIndex].recentSampleCrossings(angleCrossingWindow)
	chCrossings := st.freq[ch].recentSampleCrossings(angleCrossingWindow)
	if len(baseCrossings) == 0 || len(chCrossings) == 0 || baseSamplePeriod <= 0 {
		return 0, false
	}
	n := len(baseCrossings)
	if len(chCrossings) < n {
		n = len(chCrossings)
	}
	if n == 0 {
		return 0, false
	}
	baseStart := len(baseCrossings) - n
	chStart := len(chCrossings) - n
	var sum float64
	for i := 0; i < n; i++ {
		delta := chCrossings[chStart+i] - baseCrossings[baseStart+i]
		for delta > baseSamplePeriod/2 {
			delta -= baseSamplePeriod
		}
		for delta <= -baseSamplePeriod/2 {
			delta += baseSamplePeriod
		}
		sum += delta
	}
	return normalizeAngleDegrees(sum / float64(n) / baseSamplePeriod * 360), true
}

func (a *angleStability) accept(angle float64) float64 {
	a.goodCount++
	a.badCount = 0
	if a.goodCount >= angleGoodLimit {
		a.unstable = false
	}
	a.lastAngle = angle
	a.hasAngle = true
	return angle
}

func (a *angleStability) reject() (float64, bool) {
	a.badCount++
	a.goodCount = 0
	if a.badCount >= angleBadLimit {
		a.unstable = true
	}
	if a.unstable || !a.hasAngle {
		return math.NaN(), false
	}
	return a.lastAngle, true
}

func normalizeAngleDegrees(v float64) float64 {
	for v > 180 {
		v -= 360
	}
	for v <= -180 {
		v += 360
	}
	return v
}

// watchdog checks stale streams and publishes periodic statistics.
func (s *Service) watchdog(ctx context.Context) {
	tick := time.NewTicker(watchdogTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.checkStale(now)
			s.emitSnapshots(now)
			s.emitStats(now)
		}
	}
}

func (s *Service) checkStale(now time.Time) {
	type lostItem struct {
		sub  *sv.Subscription
		asdu *sv.ASDU
	}
	var lost []lostItem

	s.mu.Lock()
	for _, st := range s.streams {
		if !st.seen || st.lost {
			continue
		}
		if now.Sub(st.lastSeenAt) > streamLostTimeout {
			st.lost = true
			lost = append(lost, lostItem{sub: st.sub, asdu: st.lastASDU})
		}
	}
	s.mu.Unlock()

	for _, it := range lost {
		s.sink.OnSVEvent(Event{
			Reason:       ReasonStreamLost,
			Subscription: it.sub.Name,
			ASDU:         it.asdu,
			ReceivedAt:   now,
		})
	}
}

func (s *Service) emitStats(now time.Time) {
	type statsItem struct {
		subName string
		stats   StreamStats
	}
	var items []statsItem

	s.mu.Lock()
	for _, st := range s.streams {
		if !st.seen || st.lost || st.lastASDU == nil || now.Sub(st.lastStatsAt) < statsInterval {
			continue
		}
		items = append(items, statsItem{
			subName: st.sub.Name,
			stats:   st.snapshotStats(),
		})
		st.lastStatsAt = now
	}
	s.mu.Unlock()

	for _, it := range items {
		stats := it.stats
		s.sink.OnSVEvent(Event{
			Reason:       ReasonStats,
			Subscription: it.subName,
			ReceivedAt:   now,
			Stats:        &stats,
		})
	}
}

func (s *Service) emitSnapshots(now time.Time) {
	type snapshotItem struct {
		subName string
		stats   StreamStats
	}
	var items []snapshotItem

	s.mu.Lock()
	for _, st := range s.streams {
		if !st.seen || st.lost || st.lastASDU == nil || now.Sub(st.lastSnapshotAt) < snapshotInterval {
			continue
		}
		items = append(items, snapshotItem{
			subName: st.sub.Name,
			stats:   st.snapshotStats(),
		})
		st.lastSnapshotAt = now
	}
	s.mu.Unlock()

	for _, it := range items {
		stats := it.stats
		s.sink.OnSVEvent(Event{
			Reason:       ReasonSnapshot,
			Subscription: it.subName,
			ReceivedAt:   now,
			Stats:        &stats,
		})
	}
}

func (st *streamState) snapshotStats() StreamStats {
	measurements := st.measurements()
	stats := StreamStats{
		SamplesReceived: st.samplesReceived,
		GapEvents:       st.gapEvents,
		MissedTotal:     st.missedTotal,
		LastSmpCnt:      st.lastSmpCnt,
		LastConfRev:     st.lastConfRev,
		SmpSynch:        st.lastSynch,
		Simulation:      st.lastASDU.Simulation,
		SmpRate:         effectiveSmpRate(st.lastASDU, st.sub),
		FrequencyHz:     st.frequencies(),
		Instant:         instantValues(st.lastASDU),
		RMS:             measurements.rms,
		Angle:           measurements.angle,
		Quality:         st.lastASDU.Quality,
	}
	st.latestStats = &stats
	st.measuredAt = st.lastSeenAt
	st.rmsReady = st.window.count >= int(effectiveSmpRate(st.lastASDU, st.sub))
	return stats
}

func (st *streamState) frequencies() [sv.NumChannels]float64 {
	var out [sv.NumChannels]float64
	for ch := 0; ch < sv.NumChannels; ch++ {
		out[ch] = st.freq[ch].valueHz
	}
	return out
}

func instantValues(asdu *sv.ASDU) [sv.NumChannels]float64 {
	var out [sv.NumChannels]float64
	if asdu == nil {
		return out
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		out[ch] = asdu.PhysicalValue(ch)
	}
	return out
}

type noopSink struct{}

func (noopSink) OnSVEvent(Event) {}
