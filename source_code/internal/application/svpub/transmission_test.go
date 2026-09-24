package svpub

import (
	"errors"
	"testing"
	"time"

	"pbmt/internal/domain/sv"
)

type sendBoundarySink struct {
	before, after time.Time
	err           error
	hook          func()
}

func (s *sendBoundarySink) WriteFrame([]byte) error {
	s.before = time.Now()
	if s.hook != nil {
		s.hook()
	}
	s.after = time.Now()
	return s.err
}

func TestTrackedWaveformAcknowledgesOnlyItsOwnFrame(t *testing.T) {
	svc := newTestService(&mockSink{})
	settings := svc.Snapshot().Settings
	settings[0].RMS = 1200
	ch, err := svc.SetWaveformTracked(settings, false, sv.SmpSynchGlobal)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("acknowledged before send")
	default:
	}
	sink := &sendBoundarySink{}
	svc.sink = sink
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.Err != nil || r.At.IsZero() || r.At.After(sink.before) || r.AppTime.IsZero() {
		t.Fatal("wrong send boundary", r)
	}
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("duplicate receipt")
	default:
	}
}

func TestInFlightOldFrameCannotAcknowledgeNewWaveform(t *testing.T) {
	svc := newTestService(&mockSink{})
	settings := svc.Snapshot().Settings
	var ch <-chan SendReceipt
	sink := &sendBoundarySink{hook: func() {
		var err error
		settings[0].RMS = 1200
		ch, err = svc.SetWaveformTracked(settings, false, sv.SmpSynchNone)
		if err != nil {
			t.Fatal(err)
		}
	}}
	svc.sink = sink
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("old frame acknowledged new settings")
	default:
	}
	sink.hook = nil
	if err := svc.transmit(); err != nil {
		t.Fatal(err)
	}
	if r := <-ch; r.Err != nil {
		t.Fatal(r.Err)
	}
}

func TestTrackedSendFailureAndSupersession(t *testing.T) {
	svc := newTestService(&mockSink{})
	settings := svc.Snapshot().Settings
	first, err := svc.SetWaveformTracked(settings, false, sv.SmpSynchNone)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.SetWaveformTracked(settings, false, sv.SmpSynchNone)
	if err != nil {
		t.Fatal(err)
	}
	if (<-first).Err == nil {
		t.Fatal("superseded waveform reported success")
	}
	svc.sink = &sendBoundarySink{err: errors.New("injection failed")}
	if svc.transmit() == nil {
		t.Fatal("write error hidden")
	}
	if (<-second).Err == nil {
		t.Fatal("failed write yielded successful receipt")
	}
}
