package goosesub

import (
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

func TestObservationRetainsShortPulseIndependentlyOfGUI(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*goose.Subscription{sub}, nil, nil)
	at := time.Now()
	p := makePDU(1, 0, 1)
	p.AllData = []goose.DataValue{{Type: goose.DataTypeBoolean}}
	svc.handleMatch(sub, p, ethernet.Frame{Timestamp: at, ObservedAt: at})
	w := svc.Observe(sub.Name)
	defer w.Cancel()
	if len(w.Initial) != 1 || !w.Initial[0].ObservedAt.Equal(at) {
		t.Fatal("missing atomic baseline")
	}
	w.Initial[0].PDU.AllData[0].Bool = true
	if svc.Snapshots(sub.Name, at)[0].PDU.AllData[0].Bool {
		t.Fatal("baseline aliases live state")
	}
	for i, value := range []bool{true, false} {
		p := makePDU(uint32(i+2), 0, 1)
		p.AllData = []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: value}}
		now := at.Add(time.Duration(i+1) * time.Millisecond)
		svc.handleMatch(sub, p, ethernet.Frame{Timestamp: now.Add(time.Hour), ObservedAt: now})
	}
	for i, want := range []bool{true, false} {
		r := <-w.Frames
		if r.PDU.AllData[0].Bool != want || r.At.Sub(at) != time.Duration(i+1)*time.Millisecond {
			t.Fatal("pulse or monotonic timestamp lost")
		}
	}
	w.Cancel()
	w.Cancel()
	if len(svc.watches) != 0 {
		t.Fatal("observer leaked")
	}
}

func TestObservationOverflowAndReceiverCloseAreExplicit(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*goose.Subscription{sub}, nil, nil)
	w := svc.Observe(sub.Name)
	defer w.Cancel()
	for i := 0; i < 300; i++ {
		now := time.Now()
		svc.handleMatch(sub, makePDU(1, uint32(i), 1), ethernet.Frame{Timestamp: now, ObservedAt: now})
	}
	select {
	case <-w.Failed:
	default:
		t.Fatal("overflow was silent")
	}
	if len(w.Frames) != 256 {
		t.Fatal("unbounded observation queue")
	}
	svc.closeWatches()
	select {
	case <-w.Closed:
	default:
		t.Fatal("receiver closure not signaled")
	}
	w.Cancel()
}
