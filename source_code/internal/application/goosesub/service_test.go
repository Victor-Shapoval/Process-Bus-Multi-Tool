package goosesub

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

// fakeSource is a test implementation of ethernet.Source. Frames are unnecessary
// because tests call handleMatch directly.
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

// collectSink accumulates events for assertions.
type collectSink struct {
	mu     sync.Mutex
	events []Event
}

func (c *collectSink) OnGooseEvent(e Event) {
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

func testSub() *goose.Subscription {
	mac, _ := net.ParseMAC("01:0c:cd:01:00:01")
	return &goose.Subscription{
		Name:    "test",
		DstMAC:  mac,
		AppID:   0x0001,
		GocbRef: "X/LLN0$GO$cb",
	}
}

func makePDU(stNum, sqNum, confRev uint32) *goose.PDU {
	mac, _ := net.ParseMAC("01:0c:cd:01:00:01")
	return &goose.PDU{
		DstMAC:              mac,
		AppID:               0x0001,
		GocbRef:             "X/LLN0$GO$cb",
		StNum:               stNum,
		SqNum:               sqNum,
		ConfRev:             confRev,
		TimeAllowedToLiveMs: 200,
	}
}

func TestStateMachine(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	now := time.Now()

	// 1. first_seen
	svc.handleMatch(sub, makePDU(10, 0, 1), ethernet.Frame{Timestamp: now})
	// 2. heartbeat (same stNum, increasing sqNum) produces no event
	svc.handleMatch(sub, makePDU(10, 1, 1), ethernet.Frame{Timestamp: now})
	// 3. duplicate (same stNum and sqNum) produces no event
	svc.handleMatch(sub, makePDU(10, 1, 1), ethernet.Frame{Timestamp: now})
	// 4. state_change (stNum increased)
	svc.handleMatch(sub, makePDU(11, 0, 1), ethernet.Frame{Timestamp: now})
	// 5. conf_rev_changed (stNum unchanged, confRev changed)
	//    This case uses stNum=11, sqNum=1, confRev=2.
	svc.handleMatch(sub, makePDU(11, 1, 2), ethernet.Frame{Timestamp: now})
	// 6. publisher_restart (stNum decreased sharply)
	svc.handleMatch(sub, makePDU(3, 0, 2), ethernet.Frame{Timestamp: now})

	got := sink.snapshot()
	wantReasons := []EventReason{
		ReasonFirstSeen,
		ReasonStateChange,
		ReasonConfRevChanged,
		ReasonPublisherRestart,
	}
	if len(got) != len(wantReasons) {
		t.Fatalf("events: got %d, want %d: %+v", len(got), len(wantReasons), reasonsOf(got))
	}
	for i, r := range wantReasons {
		if got[i].Reason != r {
			t.Errorf("events[%d].Reason = %s, want %s", i, got[i].Reason, r)
		}
	}

	// state_change must contain prev_st_num=10.
	if got[1].PreviousStNum != 10 {
		t.Errorf("state_change.PreviousStNum = %d, want 10", got[1].PreviousStNum)
	}
	// publisher_restart must contain prev_st_num=11.
	if got[3].PreviousStNum != 11 {
		t.Errorf("publisher_restart.PreviousStNum = %d, want 11", got[3].PreviousStNum)
	}
}

func TestSqNumGapDetection(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	now := time.Now()

	svc.handleMatch(sub, makePDU(5, 0, 1), ethernet.Frame{Timestamp: now}) // first_seen
	svc.handleMatch(sub, makePDU(5, 5, 1), ethernet.Frame{Timestamp: now}) // heartbeat, gap=4
	svc.handleMatch(sub, makePDU(6, 0, 1), ethernet.Frame{Timestamp: now}) // state_change; a new stNum makes the previous gap irrelevant
	svc.handleMatch(sub, makePDU(6, 3, 1), ethernet.Frame{Timestamp: now}) // heartbeat, gap=2
	// For a state_change with the same stNum, calculate the gap relative to the previous sqNum.
	// Here stNum changed between #4 and #5, so the gap does not apply to the event.

	ev := sink.snapshot()
	// Expect first_seen (#1) and state_change (#3).
	if len(ev) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(ev), reasonsOf(ev))
	}
	// state_change: previous sqNum=5 and a new stNum. The event gap is not
	// sqNum(0)-sqNum(5)-1 = -6 because sqGap is calculated only within the same stNum.
	// A new stNum therefore has sqGap=0.
	if ev[1].Reason != ReasonStateChange {
		t.Fatalf("want state_change, got %s", ev[1].Reason)
	}
	if ev[1].SqNumGap != 0 {
		t.Errorf("new stNum: SqNumGap = %d, want 0", ev[1].SqNumGap)
	}
}

func TestReorderedHeartbeatDoesNotMoveAcceptedStateBackward(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	now := time.Now()

	svc.handleMatch(sub, makePDU(5, 5, 1), ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, makePDU(5, 4, 1), ethernet.Frame{Timestamp: now.Add(time.Millisecond)})
	svc.handleMatch(sub, makePDU(5, 6, 1), ethernet.Frame{Timestamp: now.Add(2 * time.Millisecond)})

	key := streamKey{dstMAC: makePDU(5, 0, 1).DstMAC.String(), appID: 1, gocbRef: "X/LLN0$GO$cb"}
	state := svc.streams[key]
	if state.lastSqNum != 6 {
		t.Fatalf("reordered heartbeat moved baseline: last sqNum=%d", state.lastSqNum)
	}
}

func TestOlderStateTimestampIsNotAcceptedAsPublisherRestart(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	now := time.Now().UTC()

	current := makePDU(10, 0, 1)
	current.Timestamp = now
	stale := makePDU(9, 0, 1)
	stale.Timestamp = now.Add(-time.Second)
	svc.handleMatch(sub, current, ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, stale, ethernet.Frame{Timestamp: now.Add(time.Millisecond)})

	if countReason(sink.snapshot(), ReasonPublisherRestart) != 0 {
		t.Fatal("older state was reported as publisher restart")
	}
	key := streamKey{dstMAC: current.DstMAC.String(), appID: current.AppID, gocbRef: current.GocbRef}
	if got := svc.streams[key].lastStNum; got != 10 {
		t.Fatalf("older state replaced accepted state: got stNum=%d", got)
	}
}

func TestExactReplayDoesNotKeepStreamAliveOrRestoreIt(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	t0 := time.Now()
	pdu := makePDU(1, 0, 1)
	pdu.Timestamp = t0

	svc.handleMatch(sub, pdu, ethernet.Frame{Timestamp: t0})
	svc.handleMatch(sub, pdu, ethernet.Frame{Timestamp: t0.Add(150 * time.Millisecond)})
	svc.checkStale(t0.Add(250 * time.Millisecond))
	if countReason(sink.snapshot(), ReasonStreamLost) != 1 {
		t.Fatal("exact replay incorrectly extended TimeAllowedToLive")
	}
	svc.handleMatch(sub, pdu, ethernet.Frame{Timestamp: t0.Add(300 * time.Millisecond)})
	if countReason(sink.snapshot(), ReasonStreamRestored) != 0 {
		t.Fatal("exact replay incorrectly restored a lost stream")
	}
}

func TestWatchdogStreamLostRestored(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)

	t0 := time.Now()
	svc.handleMatch(sub, makePDU(1, 0, 1), ethernet.Frame{Timestamp: t0}) // first_seen, TAL=200ms

	// Run the watchdog at t0 + 100 ms; the stream is still alive.
	svc.checkStale(t0.Add(100 * time.Millisecond))
	if gotLost(sink.snapshot()) {
		t.Fatal("stream should not be lost at TAL/2")
	}

	// At t0 + 300 ms, TAL has expired and stream_lost is expected.
	svc.checkStale(t0.Add(300 * time.Millisecond))
	ev := sink.snapshot()
	if !gotLost(ev) {
		t.Fatalf("expected stream_lost after TAL, got %+v", reasonsOf(ev))
	}

	// A repeated checkStale must not duplicate the lost event.
	svc.checkStale(t0.Add(500 * time.Millisecond))
	if countReason(sink.snapshot(), ReasonStreamLost) != 1 {
		t.Fatal("stream_lost must be emitted once")
	}

	// A received frame produces stream_restored followed by state_change.
	svc.handleMatch(sub, makePDU(2, 0, 1), ethernet.Frame{Timestamp: t0.Add(600 * time.Millisecond)})
	ev = sink.snapshot()
	if countReason(ev, ReasonStreamRestored) != 1 {
		t.Fatalf("expected stream_restored, got %+v", reasonsOf(ev))
	}
	if countReason(ev, ReasonStateChange) != 1 {
		t.Fatalf("expected state_change after restore, got %+v", reasonsOf(ev))
	}
	// Order: restored, then state_change.
	last2 := ev[len(ev)-2:]
	if last2[0].Reason != ReasonStreamRestored || last2[1].Reason != ReasonStateChange {
		t.Errorf("order wrong: %+v", reasonsOf(last2))
	}
}

func TestZeroTimeAllowedToLiveExpires(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	t0 := time.Now()
	pdu := makePDU(1, 0, 1)
	pdu.TimeAllowedToLiveMs = 0

	svc.handleMatch(sub, pdu, ethernet.Frame{Timestamp: t0})
	svc.checkStale(t0.Add(time.Nanosecond))
	if countReason(sink.snapshot(), ReasonStreamLost) != 1 {
		t.Fatal("zero TimeAllowedToLive did not expire")
	}
}

func TestRunRejectsEmptySubscriptions(t *testing.T) {
	svc := New(newFakeSource(), nil, nil, nil)
	err := svc.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for empty subscriptions")
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
	svc := New(source, []*goose.Subscription{testSub()}, nil, nil)
	if err := svc.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error: want %v, got %v", want, err)
	}
}

func TestProcessIgnoresTestMessagesUnlessExplicitlyAccepted(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	pdu := makePDU(1, 0, 1)
	pdu.Test = true

	svc.process(ethernet.Frame{Data: goose.Encode(pdu), Timestamp: time.Now()})
	if got := len(sink.snapshot()); got != 0 {
		t.Fatalf("operational subscription accepted test message: %d events", got)
	}

	sub.AcceptTest = true
	svc.process(ethernet.Frame{Data: goose.Encode(pdu), Timestamp: time.Now()})
	if got := len(sink.snapshot()); got != 1 {
		t.Fatalf("diagnostic subscription did not accept test message: %d events", got)
	}
}

func TestZeroCaptureTimestampUsesReceiveTime(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	before := time.Now()
	svc.handleMatch(sub, makePDU(1, 0, 1), ethernet.Frame{})

	events := sink.snapshot()
	if len(events) != 1 || events[0].ReceivedAt.Before(before) || events[0].ReceivedAt.IsZero() {
		t.Fatalf("invalid receive timestamp: %+v", events)
	}
	if got := svc.streams[streamKey{dstMAC: sub.DstMAC.String(), appID: sub.AppID, gocbRef: sub.GocbRef}].lastSeenAt; got.IsZero() {
		t.Fatal("watchdog timestamp was not initialized")
	}
}

func TestStateNumberWrapIsStateChange(t *testing.T) {
	sub := testSub()
	sink := &collectSink{}
	svc := New(newFakeSource(), []*goose.Subscription{sub}, sink, nil)
	now := time.Now()
	svc.handleMatch(sub, makePDU(^uint32(0), 0, 1), ethernet.Frame{Timestamp: now})
	svc.handleMatch(sub, makePDU(1, 0, 1), ethernet.Frame{Timestamp: now.Add(time.Millisecond)})

	events := sink.snapshot()
	if len(events) != 2 || events[1].Reason != ReasonStateChange {
		t.Fatalf("wrap must be a state change, got %v", reasonsOf(events))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

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

func gotLost(evs []Event) bool { return countReason(evs, ReasonStreamLost) > 0 }
