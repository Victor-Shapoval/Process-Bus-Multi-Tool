package goosepub

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/goose"
)

type fixedTimeSource struct {
	now    time.Time
	status appclock.SourceStatus
}

func (s *fixedTimeSource) Now() time.Time                      { return s.now }
func (s *fixedTimeSource) SourceStatus() appclock.SourceStatus { return s.status }

type mockSink struct {
	mu     sync.Mutex
	frames [][]byte
	times  []time.Time
}

func (m *mockSink) WriteFrame(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frames = append(m.frames, append([]byte(nil), frame...))
	m.times = append(m.times, time.Now())
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
	return append([]byte(nil), m.frames[i]...)
}

func (m *mockSink) getTime(i int) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.times[i]
}

func newTestService(sink FrameSink) *Service {
	return newTestServiceWithIntervals(sink, 2, 100)
}

func newTestServiceWithIntervals(sink FrameSink, minMs, maxMs uint32) *Service {
	cfg := PublisherConfig{
		Name:    "test_pub",
		DstMAC:  net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:  net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		AppID:   0x0001,
		GocbRef: "IED/LLN0$GO$Test",
		DatSet:  "IED/LLN0$DS1",
		GoID:    "TEST1",
		ConfRev: 1,
		MinMs:   minMs,
		MaxMs:   maxMs,
		InitialData: []goose.DataValue{
			{Type: goose.DataTypeBoolean, Bool: false},
		},
	}
	return New(cfg, sink, slog.Default(), nil)
}

// transmit keeps tests focused on the send result while the runtime uses
// transmitState to track the state number written to the wire.
func (s *Service) transmit() error {
	_, err := s.transmitState()
	return err
}

type failingSink struct{ err error }

func (s failingSink) WriteFrame([]byte) error { return s.err }

func TestNewInitializesState(t *testing.T) {
	svc := newTestService(&mockSink{})

	if svc.stNum != 1 {
		t.Errorf("stNum: want 1, got %d", svc.stNum)
	}
	if svc.sqNum != 0 {
		t.Errorf("sqNum: want 0, got %d", svc.sqNum)
	}
	if len(svc.data) != 1 {
		t.Errorf("data len: want 1, got %d", len(svc.data))
	}
	if svc.stateChanged.IsZero() {
		t.Error("state change timestamp is zero")
	}
}

func TestApplyStateIsAtomicAndIgnoresUnchangedState(t *testing.T) {
	svc := newTestService(&mockSink{})
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}

	initial := []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: false}}
	if svc.ApplyState(initial, false, false) {
		t.Fatal("unchanged state reported as changed")
	}
	if svc.stNum != 1 || svc.sqNum != 2 {
		t.Fatalf("unchanged counters: stNum=%d sqNum=%d", svc.stNum, svc.sqNum)
	}
	if len(svc.stateChangeCh) != 0 {
		t.Fatal("unchanged state scheduled a retransmission")
	}

	previousTimestamp := svc.stateChanged
	time.Sleep(time.Millisecond)
	updated := []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}
	if !svc.ApplyState(updated, true, true) {
		t.Fatal("changed state was ignored")
	}
	if svc.stNum != 2 || svc.sqNum != 0 {
		t.Fatalf("changed counters: want stNum=2 sqNum=0, got stNum=%d sqNum=%d", svc.stNum, svc.sqNum)
	}
	if !svc.test || !svc.sim {
		t.Fatalf("flags were not applied atomically: test=%v simulation=%v", svc.test, svc.sim)
	}
	if !svc.stateChanged.After(previousTimestamp) {
		t.Fatal("state change timestamp was not updated")
	}
	if len(svc.stateChangeCh) != 1 {
		t.Fatal("state change did not schedule one retransmission sequence")
	}

	if svc.ApplyState(updated, true, true) {
		t.Fatal("duplicate state reported as changed")
	}
	if svc.stNum != 2 {
		t.Fatalf("duplicate state incremented stNum to %d", svc.stNum)
	}
}

func TestApplyStateDeepCopiesData(t *testing.T) {
	svc := newTestService(&mockSink{})
	data := []goose.DataValue{{
		Type:  goose.DataTypeStructure,
		Bytes: []byte{1, 2},
		Children: []goose.DataValue{{
			Type:  goose.DataTypeBitString,
			Bytes: []byte{3, 4},
		}},
	}}
	if !svc.ApplyState(data, false, false) {
		t.Fatal("changed state was ignored")
	}
	data[0].Bytes[0] = 9
	data[0].Children[0].Bytes[0] = 9

	if svc.data[0].Bytes[0] != 1 || svc.data[0].Children[0].Bytes[0] != 3 ||
		svc.logicalData[0].Bytes[0] != 1 || svc.logicalData[0].Children[0].Bytes[0] != 3 {
		t.Fatal("ApplyState retained aliases to caller data")
	}
}

func TestTransmitEncodesCountersAndStableTimestamp(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)

	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}

	first := decodeFrame(t, sink.getFrame(0))
	second := decodeFrame(t, sink.getFrame(1))
	if first.GocbRef != "IED/LLN0$GO$Test" || first.AppID != 0x0001 {
		t.Fatalf("unexpected identity: gocbRef=%q appID=0x%04X", first.GocbRef, first.AppID)
	}
	if first.StNum != 1 || first.SqNum != 0 || second.StNum != 1 || second.SqNum != 1 {
		t.Fatalf("unexpected counters: first=%d/%d second=%d/%d", first.StNum, first.SqNum, second.StNum, second.SqNum)
	}
	if !first.Timestamp.Equal(second.Timestamp) {
		t.Fatalf("timestamp changed during retransmission: %s != %s", first.Timestamp, second.Timestamp)
	}
}

func TestPublisherUsesApplicationClockForStateTimestamp(t *testing.T) {
	sink := &mockSink{}
	source := &fixedTimeSource{
		now:    time.Date(2026, 8, 5, 12, 30, 15, 125_000_000, time.UTC),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	cfg := PublisherConfig{
		DstMAC:      net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:      net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		InitialData: []goose.DataValue{{Type: goose.DataTypeBoolean}},
	}
	svc := New(cfg, sink, slog.Default(), source)
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	pdu := decodeFrame(t, sink.getFrame(0))
	if delta := pdu.Timestamp.Sub(source.now); delta < -time.Microsecond || delta > time.Microsecond {
		t.Fatalf("GOOSE timestamp: want %v, got %v", source.now, pdu.Timestamp)
	}
	if pdu.TimeQuality.ClockNotSynchronized {
		t.Fatal("synchronized PTP clock was marked unsynchronized")
	}

	source.now = source.now.Add(time.Second)
	source.status.Synchronized = false
	if !svc.ApplyState([]goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}, false, false) {
		t.Fatal("changed state was ignored")
	}
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	pdu = decodeFrame(t, sink.getFrame(1))
	if delta := pdu.Timestamp.Sub(source.now); delta < -time.Microsecond || delta > time.Microsecond {
		t.Fatalf("updated GOOSE timestamp: want %v, got %v", source.now, pdu.Timestamp)
	}
	if !pdu.TimeQuality.ClockNotSynchronized {
		t.Fatal("unlocked PTP clock was not marked unsynchronized")
	}
}

func TestPublisherDoesNotClaimUnverifiedSystemClockQuality(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	pdu := decodeFrame(t, sink.getFrame(0))
	if !pdu.TimeQuality.ClockNotSynchronized || pdu.TimeQuality.LeapSecondsKnown || pdu.TimeQuality.Accuracy != goose.AccuracyUnspecified {
		t.Fatalf("unverified clock quality was overstated: %+v", pdu.TimeQuality)
	}
}

func TestZeroDatasetUTCTimeUsesApplicationClock(t *testing.T) {
	sink := &mockSink{}
	source := &fixedTimeSource{
		now:    time.Date(2026, 8, 5, 12, 30, 15, 125_000_000, time.UTC),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	cfg := PublisherConfig{
		DstMAC:      net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:      net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		InitialData: []goose.DataValue{{Type: goose.DataTypeUTCTime}},
	}
	svc := New(cfg, sink, slog.Default(), source)
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	pdu := decodeFrame(t, sink.getFrame(0))
	if len(pdu.AllData) != 1 || !pdu.AllData[0].Time.Equal(source.now) {
		t.Fatalf("dataset UTC time: want %v, got %+v", source.now, pdu.AllData)
	}
	if pdu.AllData[0].TimeQuality.ClockNotSynchronized || pdu.AllData[0].TimeQuality.Accuracy != goose.AccuracyUnspecified {
		t.Fatalf("dataset UTC time quality: %+v", pdu.AllData[0].TimeQuality)
	}
}

func TestAutoDatasetUTCTimeChangesOnlyWithLogicalState(t *testing.T) {
	source := &fixedTimeSource{
		now:    time.Date(2026, 8, 5, 12, 30, 15, 125_000_000, time.UTC),
		status: appclock.SourceStatus{PTPEnabled: true, Synchronized: true},
	}
	raw := []goose.DataValue{{Type: goose.DataTypeUTCTime}}
	svc := New(PublisherConfig{InitialData: raw}, &mockSink{}, slog.Default(), source)
	initialTime := svc.data[0].Time

	source.now = source.now.Add(time.Second)
	if svc.ApplyState(raw, false, false) {
		t.Fatal("unchanged automatic UTC time incremented stNum")
	}
	if svc.stNum != 1 || !svc.data[0].Time.Equal(initialTime) {
		t.Fatalf("unchanged state was mutated: stNum=%d time=%v", svc.stNum, svc.data[0].Time)
	}

	// A real state change stamps both GOOSE t and automatic dataset UtcTime
	// from the same PTP-disciplined application clock.
	if !svc.ApplyState(raw, true, false) {
		t.Fatal("changed test flag was ignored")
	}
	if !svc.stateChanged.Equal(source.now) || !svc.data[0].Time.Equal(source.now) {
		t.Fatalf("PTP time was not applied consistently: t=%v dataset=%v want=%v", svc.stateChanged, svc.data[0].Time, source.now)
	}
}

func TestDatasetUTCTimeDoesNotClaimSystemClockSynchronization(t *testing.T) {
	sink := &mockSink{}
	cfg := PublisherConfig{
		DstMAC:      net.HardwareAddr{0x01, 0x0C, 0xCD, 0x01, 0x00, 0x01},
		SrcMAC:      net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		InitialData: []goose.DataValue{{Type: goose.DataTypeUTCTime}},
	}
	svc := New(cfg, sink, slog.Default(), nil)
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	quality := decodeFrame(t, sink.getFrame(0)).AllData[0].TimeQuality
	if !quality.ClockNotSynchronized || quality.LeapSecondsKnown || quality.Accuracy != goose.AccuracyUnspecified {
		t.Fatalf("dataset UTC time quality was overstated: %+v", quality)
	}
}

func TestCountersSkipZeroOnOverflow(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	svc.stNum = math.MaxUint32
	if !svc.ApplyState([]goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}, false, false) {
		t.Fatal("changed state was ignored")
	}
	if svc.stNum != 1 {
		t.Fatalf("stNum overflow: want 1, got %d", svc.stNum)
	}

	svc.sqNum = math.MaxUint32
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	if svc.sqNum != 1 {
		t.Fatalf("sqNum overflow: want next value 1, got %d", svc.sqNum)
	}
}

func TestRunSendsHeartbeat(t *testing.T) {
	sink := &mockSink{}
	svc := newTestService(sink)
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := sink.count(); got < 4 {
		t.Errorf("expected initial frame and at least 3 heartbeats, got %d frames", got)
	}
}

func TestRunReturnsWriteError(t *testing.T) {
	want := errors.New("write failed")
	svc := newTestService(failingSink{err: want})
	if err := svc.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error: want %v, got %v", want, err)
	}
}

func TestStateChangeSendsImmediatelyThenWaitsMinInterval(t *testing.T) {
	sink := &mockSink{}
	svc := newTestServiceWithIntervals(sink, 30, 200)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run failed: %v", err)
		}
	}()

	waitForFrames(t, sink, 1, 100*time.Millisecond)
	changedAt := time.Now()
	svc.ApplyState([]goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}, false, false)
	waitForFrames(t, sink, 2, 100*time.Millisecond)
	if delay := sink.getTime(1).Sub(changedAt); delay > 50*time.Millisecond {
		t.Fatalf("first state-change frame was not immediate: %v", delay)
	}

	time.Sleep(15 * time.Millisecond)
	if got := sink.count(); got != 2 {
		t.Fatalf("retransmission was sent before MinTime: got %d frames", got)
	}
	waitForFrames(t, sink, 3, 70*time.Millisecond)
	if interval := sink.getTime(2).Sub(sink.getTime(1)); interval < 25*time.Millisecond {
		t.Fatalf("first retransmission interval %v is shorter than MinTime", interval)
	}

	first := decodeFrame(t, sink.getFrame(1))
	second := decodeFrame(t, sink.getFrame(2))
	if first.StNum != 2 || first.SqNum != 0 || second.StNum != 2 || second.SqNum != 1 {
		t.Fatalf("unexpected state-change counters: first=%d/%d second=%d/%d", first.StNum, first.SqNum, second.StNum, second.SqNum)
	}
	if !first.Timestamp.Equal(second.Timestamp) {
		t.Fatal("state-change timestamp differs between retransmissions")
	}
}

func TestNextGooseInterval(t *testing.T) {
	maximum := 100 * time.Millisecond
	tests := []struct {
		current time.Duration
		want    time.Duration
	}{
		{2 * time.Millisecond, 4 * time.Millisecond},
		{50 * time.Millisecond, 100 * time.Millisecond},
		{60 * time.Millisecond, 100 * time.Millisecond},
		{100 * time.Millisecond, 100 * time.Millisecond},
	}
	for _, tc := range tests {
		if got := nextGooseInterval(tc.current, maximum); got != tc.want {
			t.Errorf("nextGooseInterval(%v): want %v, got %v", tc.current, tc.want, got)
		}
	}
}

func decodeFrame(t *testing.T, frame []byte) *goose.PDU {
	t.Helper()
	pdu, err := goose.Decode(frame)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	return pdu
}

func waitForFrames(t *testing.T, sink *mockSink, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sink.count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d frames; got %d", want, sink.count())
}
