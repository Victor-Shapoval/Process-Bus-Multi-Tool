package svsub

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/sv"
)

type fakeSource struct {
	frames chan ethernet.Frame
	errs   chan error
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		frames: make(chan ethernet.Frame),
		errs:   make(chan error),
	}
}
func (f *fakeSource) Frames() <-chan ethernet.Frame { return f.frames }
func (f *fakeSource) Errors() <-chan error          { return f.errs }
func (f *fakeSource) Close() error                  { close(f.frames); close(f.errs); return nil }

type collectSink struct {
	mu     sync.Mutex
	events []Event
}

func (c *collectSink) OnSVEvent(e Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}
func (c *collectSink) snapshot() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

func testSub() *sv.Subscription {
	mac, _ := net.ParseMAC("01:0c:cd:04:00:01")
	return &sv.Subscription{
		Name:   "test",
		DstMAC: mac,
		AppID:  0x4001,
		SvID:   "MU0001",
	}
}

func makeASDU(smpCnt uint16, confRev uint32, synch sv.SmpSynch) *sv.ASDU {
	mac, _ := net.ParseMAC("01:0c:cd:04:00:01")
	return &sv.ASDU{
		DstMAC:   mac,
		AppID:    0x4001,
		SvID:     "MU0001",
		SmpCnt:   smpCnt,
		ConfRev:  confRev,
		SmpSynch: synch,
	}
}

func TestSmpCntContinuity(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	// first_seen
	svc.handleMatch(sub, makeASDU(100, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})
	// continuous
	svc.handleMatch(sub, makeASDU(101, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})
	// gap: expected 102, got 105 → missed 3
	svc.handleMatch(sub, makeASDU(105, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})

	ev := sink.snapshot()
	reasons := reasonsOf(ev)
	if len(ev) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(ev), reasons)
	}
	if ev[0].Reason != ReasonFirstSeen {
		t.Errorf("ev[0] = %s, want first_seen", ev[0].Reason)
	}
	if ev[1].Reason != ReasonSmpCntGap {
		t.Errorf("ev[1] = %s, want smpcnt_gap", ev[1].Reason)
	}
	if ev[1].MissedSamples != 3 {
		t.Errorf("missed = %d, want 3", ev[1].MissedSamples)
	}
}

func TestSmpCntWrap(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	svc.handleMatch(sub, makeASDU(65534, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, makeASDU(65535, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, makeASDU(0, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})

	ev := sink.snapshot()
	// first_seen only, no gap at wrap 65535→0
	for _, e := range ev {
		if e.Reason == ReasonSmpCntGap {
			t.Errorf("unexpected gap at wrap: expected=%d actual=%d missed=%d",
				e.ExpectedSmpCnt, e.ActualSmpCnt, e.MissedSamples)
		}
	}
}

func TestSmpCntSecondRollover(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	last := makeASDU(3999, 1, sv.SmpSynchGlobal)
	last.SmpRate = 80
	first := makeASDU(0, 1, sv.SmpSynchGlobal)
	first.SmpRate = 80
	svc.handleMatch(sub, last, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now.Add(250 * time.Microsecond)})

	if countReason(sink.snapshot(), ReasonSmpCntGap) != 0 {
		t.Fatal("normal 4000 -> 0 rollover reported as a gap")
	}
}

func TestSmpCntGapAtSecondRollover(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	last := makeASDU(3998, 1, sv.SmpSynchGlobal)
	last.SmpRate = 80
	first := makeASDU(0, 1, sv.SmpSynchGlobal)
	first.SmpRate = 80
	svc.handleMatch(sub, last, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now.Add(500 * time.Microsecond)})

	events := sink.snapshot()
	if countReason(events, ReasonSmpCntGap) != 1 {
		t.Fatalf("expected one rollover gap, got %v", reasonsOf(events))
	}
	if events[1].MissedSamples != 1 {
		t.Fatalf("missed samples: want 1, got %d", events[1].MissedSamples)
	}
}

func TestSmpCntGapAt60HzRollover(t *testing.T) {
	sub := testSub()
	sub.SampleTimingFrequency = 60
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	last := makeASDU(4798, 1, sv.SmpSynchGlobal)
	last.SmpRate = 80
	first := makeASDU(0, 1, sv.SmpSynchGlobal)
	first.SmpRate = 80
	svc.handleMatch(sub, last, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now.Add(500 * time.Microsecond)})

	events := sink.snapshot()
	if countReason(events, ReasonSmpCntGap) != 1 || events[1].MissedSamples != 1 {
		t.Fatalf("60 Hz rollover gap not detected: %+v", events)
	}
}

func TestConfRevAndSyncChange(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	now := time.Now()

	svc.handleMatch(sub, makeASDU(0, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, makeASDU(1, 2, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now}) // confRev changed
	svc.handleMatch(sub, makeASDU(2, 2, sv.SmpSynchNone), ethernet.Frame{Timestamp: now})   // sync changed

	ev := sink.snapshot()
	if countReason(ev, ReasonConfRevChanged) != 1 {
		t.Errorf("expected 1 conf_rev_changed, got %v", reasonsOf(ev))
	}
	if countReason(ev, ReasonSyncChanged) != 1 {
		t.Errorf("expected 1 sync_changed, got %v", reasonsOf(ev))
	}
}

func TestDuplicateAndReorderedSamplesDoNotMutateMeasurements(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*sv.Subscription{sub}, &collectSink{}, nil)
	now := time.Now()

	first := makeASDU(5, 1, sv.SmpSynchGlobal)
	first.Channels[sv.ChIa] = 1_000
	stale := makeASDU(4, 1, sv.SmpSynchGlobal)
	stale.Channels[sv.ChIa] = 999_000
	next := makeASDU(6, 1, sv.SmpSynchGlobal)
	next.Channels[sv.ChIa] = 3_000
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, stale, ethernet.Frame{Timestamp: now.Add(time.Microsecond)})
	svc.handleMatch(sub, next, ethernet.Frame{Timestamp: now.Add(2 * time.Microsecond)})

	key := streamKey{dstMAC: first.DstMAC.String(), appID: first.AppID, svID: first.SvID}
	state := svc.streams[key]
	if state.lastSmpCnt != 6 || state.samplesReceived != 2 || state.window.count != 2 {
		t.Fatalf("stale sample mutated stream state: last=%d samples=%d window=%d", state.lastSmpCnt, state.samplesReceived, state.window.count)
	}
	wantRMS := math.Sqrt((1*1 + 3*3) / 2.0)
	if got := state.window.rms()[sv.ChIa]; math.Abs(got-wantRMS) > 1e-12 {
		t.Fatalf("stale sample polluted RMS: want %g, got %g", wantRMS, got)
	}
}

func TestGapResetsThenSeedsMeasurementsWithCurrentSample(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*sv.Subscription{sub}, &collectSink{}, nil)
	now := time.Now()

	first := makeASDU(1, 1, sv.SmpSynchGlobal)
	first.Channels[sv.ChIa] = 1_000
	afterGap := makeASDU(3, 1, sv.SmpSynchGlobal)
	afterGap.Channels[sv.ChIa] = 7_000
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, afterGap, ethernet.Frame{Timestamp: now.Add(time.Microsecond)})

	key := streamKey{dstMAC: first.DstMAC.String(), appID: first.AppID, svID: first.SvID}
	state := svc.streams[key]
	if state.window.count != 1 {
		t.Fatalf("gap window: want current sample only, got %d samples", state.window.count)
	}
	if got := state.window.rms()[sv.ChIa]; got != 7 {
		t.Fatalf("gap current sample was not retained: want 7 A, got %g", got)
	}
}

func TestWatchdogStreamLost(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)

	t0 := time.Now()
	svc.handleMatch(sub, makeASDU(0, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: t0})

	// still alive at 50ms
	svc.checkStale(t0.Add(50 * time.Millisecond))
	if countReason(sink.snapshot(), ReasonStreamLost) != 0 {
		t.Fatal("should not be lost at 50ms")
	}

	// lost at 200ms (> 100ms timeout)
	svc.checkStale(t0.Add(200 * time.Millisecond))
	if countReason(sink.snapshot(), ReasonStreamLost) != 1 {
		t.Fatal("expected stream_lost")
	}

	// restore
	svc.handleMatch(sub, makeASDU(1, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: t0.Add(300 * time.Millisecond)})
	if countReason(sink.snapshot(), ReasonStreamRestored) != 1 {
		t.Fatal("expected stream_restored")
	}
}

func TestLostStreamDoesNotEmitStaleSnapshotsOrStats(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*sv.Subscription{sub}, sink, nil)
	t0 := time.Now()

	svc.handleMatch(sub, makeASDU(0, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: t0})
	svc.emitSnapshots(t0.Add(50 * time.Millisecond))
	initial := sink.snapshot()
	if countReason(initial, ReasonSnapshot) != 1 || countReason(initial, ReasonStats) != 0 {
		t.Fatal("expected initial snapshot before the statistics interval")
	}

	svc.checkStale(t0.Add(200 * time.Millisecond))
	svc.emitSnapshots(t0.Add(300 * time.Millisecond))
	svc.emitStats(t0.Add(11 * time.Second))
	events := sink.snapshot()
	if countReason(events, ReasonStreamLost) != 1 {
		t.Fatal("expected stream_lost")
	}
	if countReason(events, ReasonSnapshot) != 1 || countReason(events, ReasonStats) != 0 {
		t.Fatalf("lost stream emitted stale data: %v", reasonsOf(events))
	}

	svc.handleMatch(sub, makeASDU(1, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: t0.Add(12 * time.Second)})
	svc.emitSnapshots(t0.Add(12*time.Second + snapshotInterval))
	svc.emitStats(t0.Add(12*time.Second + statsInterval))
	restored := sink.snapshot()
	if countReason(restored, ReasonSnapshot) != 2 || countReason(restored, ReasonStats) != 1 {
		t.Fatal("restored stream did not resume snapshots and statistics")
	}
}

func TestRunRejectsEmpty(t *testing.T) {
	svc := New(newFakeSource(), nil, nil, nil)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestRunReturnsTerminalCaptureError(t *testing.T) {
	want := errors.New("capture failed")
	source := &fakeSource{
		frames: make(chan ethernet.Frame),
		errs:   make(chan error, 1),
	}
	source.errs <- want
	close(source.errs)
	close(source.frames)
	svc := New(source, []*sv.Subscription{testSub()}, nil, nil)
	if err := svc.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error: want %v, got %v", want, err)
	}
}

func TestSampleWindowRMSAfterWrap(t *testing.T) {
	var window sampleWindow
	for i := 1; i <= 40; i++ {
		asdu := &sv.ASDU{}
		asdu.Channels[sv.ChIa] = int32(i) * int32(sv.CurrentScale)
		window.add(asdu, 1, uint64(i))
	}

	var sumSquares float64
	for i := 9; i <= 40; i++ {
		sumSquares += float64(i * i)
	}
	want := math.Sqrt(sumSquares / 32)
	if got := window.rms()[sv.ChIa]; math.Abs(got-want) > 1e-12 {
		t.Fatalf("RMS after ring wrap: want %.12f, got %.12f", want, got)
	}

	between, ok := window.rmsBetween(20, 30)
	if !ok {
		t.Fatal("expected an RMS value for selected sample range")
	}
	sumSquares = 0
	for i := 21; i <= 30; i++ {
		sumSquares += float64(i * i)
	}
	want = math.Sqrt(sumSquares / 10)
	if got := between[sv.ChIa]; math.Abs(got-want) > 1e-12 {
		t.Fatalf("range RMS: want %.12f, got %.12f", want, got)
	}
}

func TestSampleWindowMeasurementsDoNotAllocate(t *testing.T) {
	var window sampleWindow
	for i := 1; i <= 80; i++ {
		asdu := &sv.ASDU{}
		asdu.Channels[sv.ChIa] = int32(i) * int32(sv.CurrentScale)
		window.add(asdu, 1, uint64(i))
	}
	allocations := testing.AllocsPerRun(100, func() {
		_ = window.rms()
		_, _ = window.rmsBetween(20, 60)
	})
	if allocations != 0 {
		t.Fatalf("measurement snapshot allocated %.2f objects; want 0", allocations)
	}
}

func reasonsOf(evs []Event) []EventReason {
	out := make([]EventReason, len(evs))
	for i, e := range evs {
		out[i] = e.Reason
	}
	return out
}

func countReason(evs []Event, r EventReason) int {
	n := 0
	for _, e := range evs {
		if e.Reason == r {
			n++
		}
	}
	return n
}
