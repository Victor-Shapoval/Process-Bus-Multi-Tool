package automation

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"pbmt/internal/application/control"
	"pbmt/internal/application/svpub"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

func ms(v int64) *int64 { return &v }

func sequenceMemory(a *API, m *memoryAgent) memoryAgent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := *m
	out.goose.Data = goose.CloneDataValues(m.goose.Data)
	return out
}

func gooseSequence(t *testing.T, a *API, delays ...int64) TimedGooseRequest {
	t.Helper()
	s := streamFor(t, a, "goose_pub")
	r := TimedGooseRequest{StreamID: s.ID, Revision: a.Catalog.Revision}
	for i, delay := range delays {
		value := json.RawMessage("true")
		if i%2 != 0 {
			value = json.RawMessage("false")
		}
		r.Steps = append(r.Steps, GooseStep{ms(delay), []GooseChange{{s.Signals[0].ID, value}}})
	}
	return r
}

func svSequence(t *testing.T, a *API, delays ...int64) TimedSVRequest {
	t.Helper()
	s := streamFor(t, a, "sv_pub")
	r := TimedSVRequest{StreamID: s.ID, Revision: a.Catalog.Revision}
	for i, delay := range delays {
		r.Steps = append(r.Steps, SVStep{ms(delay), []SVChange{{SignalID: s.Signals[0].ID, RMS: finite(float64(i+1) * 10)}}})
	}
	return r
}

func sequenceState(t *testing.T, a *API, id, state string, applied int) SequenceStatus {
	t.Helper()
	s, err := a.GetSequence(SequenceRequest{id})
	if err != nil || s.State != state || s.StepsApplied != applied {
		t.Fatalf("sequence: %+v, error=%v; want %s/%d", s, err, state, applied)
	}
	return s
}

func TestTimedSequencesExecuteAndPreserveSettings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, m := testAPI(t)
		defer a.Release()
		before := m.sv
		gr := gooseSequence(t, a, 0, 20, 30)
		sr := svSequence(t, a, 0, 25, 20)
		// Partial updates accumulate; the last step changes only phase.
		sr.Steps[2].Changes[0].RMS = nil
		sr.Steps[2].Changes[0].Phase = finite(480)
		g, err := a.TimedGooseSet(gr)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.TimedSVSet(sr)
		if err != nil {
			t.Fatal(err)
		}
		// Accepted requests must not retain mutable caller-owned arguments.
		gr.Steps[0].Changes[0].Value[0] = 'x'
		*sr.Steps[1].Changes[0].RMS = 999
		synctest.Wait()
		sequenceState(t, a, g.ID, "running", 1)
		sequenceState(t, a, s.ID, "running", 1)
		got := sequenceMemory(a, m)
		if !got.goose.Data[0].Bool || got.sv.Settings[0].RMS != 10 {
			t.Fatal("first steps not applied")
		}
		if _, err := a.GooseGet(StreamRequest{gr.StreamID}); err != nil {
			t.Fatal("sequence blocked reads", err)
		}
		if _, err := a.GooseSet(GooseSetRequest{gr.StreamID, gr.Revision, gr.Steps[1].Changes}); err == nil {
			t.Fatal("direct write bypassed sequence")
		}
		if _, err := a.SVSet(SVSetRequest{sr.StreamID, sr.Revision, sr.Steps[0].Changes}); err == nil {
			t.Fatal("direct SV write bypassed sequence")
		}
		if _, err := a.TimedSVSet(sr); err == nil {
			t.Fatal("second sequence accepted on same stream")
		}
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		got = sequenceMemory(a, m)
		if got.goose.Data[0].Bool || got.svWrites != 1 {
			t.Fatal("incorrect step timing")
		}
		time.Sleep(25 * time.Millisecond)
		synctest.Wait()
		last := sequenceState(t, a, s.ID, "completed", 3)
		if last.LastAppliedAt.Sub(s.StartedAt) != 45*time.Millisecond || last.MaxLatenessMS != 0 {
			t.Fatal("cumulative timing drift", last)
		}
		got = sequenceMemory(a, m)
		if got.sv.Settings[0].RMS != 20 || got.sv.Settings[0].PhaseDeg != 120 {
			t.Fatal("partial steps not cumulative")
		}
		for i, v := range got.sv.Settings {
			if v.Frequency != before.Settings[i].Frequency || v.Quality != before.Settings[i].Quality || (i != 0 && v != before.Settings[i]) {
				t.Fatal("unrequested SV setting changed")
			}
		}
		if got.sv.Synch != before.Synch || got.sv.Simulation != before.Simulation || !got.goose.Test || !got.goose.Simulation {
			t.Fatal("engineer flags changed")
		}
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		sequenceState(t, a, g.ID, "completed", 3)
		if !sequenceMemory(a, m).goose.Data[0].Bool {
			t.Fatal("final state not retained")
		}
		if _, err := a.SVSet(SVSetRequest{sr.StreamID, sr.Revision, sr.Steps[0].Changes}); err != nil {
			t.Fatal("completed sequence still owns stream", err)
		}
	})
}

func TestSequencePreflightHasNoWrites(t *testing.T) {
	for _, kind := range []string{"bad_value", "bad_signal", "duplicate", "revision", "empty_changes", "stopped", "missing_delay", "negative_delay", "zero_delay", "overflow", "empty", "too_many", "bad_limit", "sv_negative", "sv_phase", "sv_not_ready"} {
		t.Run(kind, func(t *testing.T) {
			a, m := testAPI(t)
			defer a.Release()
			g := gooseSequence(t, a, 0, 100)
			s := svSequence(t, a, 0, 100)
			switch kind {
			case "bad_value":
				g.Steps[1].Changes[0].Value = json.RawMessage("123")
			case "bad_signal":
				g.Steps[1].Changes[0].SignalID = "wrong"
			case "duplicate":
				g.Steps[1].Changes = append(g.Steps[1].Changes, g.Steps[1].Changes[0])
			case "revision":
				g.Revision = "old"
			case "empty_changes":
				g.Steps[1].Changes = nil
			case "stopped":
				m.state = control.StatusStopped
			case "missing_delay":
				g.Steps[1].AfterMS = nil
			case "negative_delay":
				g.Steps[1].AfterMS = ms(-1)
			case "zero_delay":
				g.Steps[1].AfterMS = ms(0)
			case "overflow":
				g.Steps[1].AfterMS = ms(math.MaxInt64)
			case "empty":
				g.Steps = nil
			case "too_many":
				g.Steps = make([]GooseStep, 1001)
			case "bad_limit":
				g.MaxLatenessMS = ms(0)
			case "sv_negative":
				s.Steps[1].Changes[0].RMS = finite(-1)
			case "sv_phase":
				v := math.Inf(1)
				s.Steps[1].Changes[0].Phase = &v
			case "sv_not_ready":
				m.sv.Ready = false
			}
			var err error
			if strings.HasPrefix(kind, "sv_") {
				_, err = a.TimedSVSet(s)
			} else {
				_, err = a.TimedGooseSet(g)
			}
			if err == nil || m.gooseWrites != 0 || m.svWrites != 0 || len(a.sequences) != 0 {
				t.Fatal("invalid sequence accepted or partly applied", err)
			}
		})
	}
}

func TestSequencePreflightChecksCumulativeMTU(t *testing.T) {
	a, m := testAPI(t)
	s := streamFor(t, a, "goose_pub")
	s.Signals[0].Type, s.Signals[1].Type = "VISIBLE_STRING", "VISIBLE_STRING"
	a.streams[s.ID] = s
	r := gooseSequence(t, a, 0, 10)
	raw, _ := json.Marshal(strings.Repeat("x", 900))
	r.Steps[0].Changes[0].Value = raw
	r.Steps[1].Changes = []GooseChange{{s.Signals[1].ID, raw}}
	if _, err := a.TimedGooseSet(r); err == nil || !strings.Contains(err.Error(), "MTU") || m.gooseWrites != 0 {
		t.Fatal("cumulative payload was not validated", err)
	}
}

func TestSequenceCancellationAndControlRelease(t *testing.T) {
	for _, how := range []string{"cancel", "release", "close", "revoke"} {
		t.Run(how, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, m := testAPI(t)
				r := gooseSequence(t, a, 0, 100)
				g, err := a.TimedGooseSet(r)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				switch how {
				case "cancel":
					for range 2 {
						if state, err := a.CancelSequence(SequenceRequest{g.ID}); err != nil || state.State != "cancelled" {
							t.Fatal("cancel failed", state, err)
						}
					}
				case "release":
					err = a.Release()
				case "close":
					err = a.Close()
				case "revoke":
					a.Revoke()
				}
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if m.gooseWrites != 1 || !m.goose.Data[0].Bool || a.sequences[g.ID].status.State == "running" {
					t.Fatal("late write after cancel/release")
				}
				if how == "close" && m.closes != 1 || how == "release" && (m.closes != 0 || m.releases != 1) {
					t.Fatal("wrong shutdown policy")
				}
				if how == "cancel" {
					if _, err := a.GooseSet(GooseSetRequest{r.StreamID, r.Revision, r.Steps[1].Changes}); err != nil {
						t.Fatal("cancel did not release stream", err)
					}
				}
			})
		})
	}
}

func TestSequenceSourceEditAndModuleStop(t *testing.T) {
	for _, how := range []string{"source", "module"} {
		t.Run(how, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, m := testAPI(t)
				r, err := a.TimedSVSet(svSequence(t, a, 0, 100))
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if how == "source" {
					if err := os.WriteFile(filepath.Join(a.root, "cfg", "goose_sub.yaml"), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					a.mu.Lock()
					m.state = control.StatusStopped
					a.mu.Unlock()
				}
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()
				sequenceState(t, a, r.ID, "failed", 1)
				if m.svWrites != 1 {
					t.Fatal("sequence wrote after source edit/module stop")
				}
			})
		})
	}
}

type delayedSequenceAgent struct {
	*memoryAgent
	delay time.Duration
	err   error
}

func (m *delayedSequenceAgent) ApplySV(name string, values [sv.NumChannels]svpub.ChannelSetting, simulation bool, synch sv.SmpSynch) error {
	time.Sleep(m.delay)
	if m.err != nil {
		return m.err
	}
	return m.memoryAgent.ApplySV(name, values, simulation, synch)
}

func TestSequenceLateAndFailedSteps(t *testing.T) {
	for _, how := range []string{"late", "backlog", "apply_error", "no_drift"} {
		t.Run(how, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, m := testAPI(t)
				d := &delayedSequenceAgent{memoryAgent: m, delay: 200 * time.Millisecond}
				if how == "backlog" {
					d.delay = 30 * time.Millisecond
				}
				if how == "apply_error" {
					d.delay, d.err = 0, errors.New("injected publisher failure")
				}
				if how == "no_drift" {
					d.delay = 3 * time.Millisecond
				}
				a.Attach(d)
				r, err := a.TimedSVSet(svSequence(t, a, 0, 10, 10))
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if how == "no_drift" {
					s := sequenceState(t, a, r.ID, "completed", 3)
					if s.LastAppliedAt.Sub(s.StartedAt) != 23*time.Millisecond || s.MaxLatenessMS != 3 {
						t.Fatal("application latency accumulated across steps", s)
					}
					return
				}
				want := 1
				if how == "apply_error" {
					want = 0
				}
				s := sequenceState(t, a, r.ID, "failed", want)
				if s.Error == "" || m.svWrites != want {
					t.Fatal("failure not reported or stale steps applied", s)
				}
			})
		})
	}
}

func TestSequenceHistoryIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _ := testAPI(t)
		active, err := a.TimedGooseSet(gooseSequence(t, a, 10000))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Release()
		for range maxSequences + 2 {
			if _, err := a.TimedSVSet(svSequence(t, a, 0)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		if len(a.sequences) != maxSequences {
			t.Fatal("unbounded history")
		}
		sequenceState(t, a, active.ID, "running", 0)
		if _, err := a.GetSequence(SequenceRequest{"sequence-2"}); err == nil {
			t.Fatal("oldest finished record was not evicted")
		}
	})
}

type blockedSequenceAgent struct {
	*memoryAgent
	entered chan struct{}
	resume  chan struct{}
}

func (m *blockedSequenceAgent) ApplySV(name string, values [sv.NumChannels]svpub.ChannelSetting, simulation bool, synch sv.SmpSynch) error {
	close(m.entered)
	<-m.resume
	return m.memoryAgent.ApplySV(name, values, simulation, synch)
}

func TestCancelSequenceWaitsForInFlightWrite(t *testing.T) {
	a, m := testAPI(t)
	defer a.Release()
	b := &blockedSequenceAgent{memoryAgent: m, entered: make(chan struct{}), resume: make(chan struct{})}
	a.Attach(b)
	req := svSequence(t, a, 0, 60000)
	req.MaxLatenessMS = ms(60000)
	run, err := a.TimedSVSet(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		close(b.resume)
		t.Fatal("write did not start")
	}
	started := make(chan struct{})
	done := make(chan SequenceStatus, 1)
	go func() {
		close(started)
		status, _ := a.CancelSequence(SequenceRequest{run.ID})
		done <- status
	}()
	<-started
	select {
	case <-done:
		close(b.resume)
		t.Fatal("cancel returned while write was still in progress")
	default:
	}
	close(b.resume)
	select {
	case status := <-done:
		if status.State != "cancelled" || status.StepsApplied != 1 {
			t.Fatal("cancel did not fence remaining steps", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel deadlocked")
	}
	if got := sequenceMemory(a, m); got.svWrites != 1 || got.sv.Settings[0].RMS != 10 {
		t.Fatal("cancel changed last applied value")
	}
}

func TestSequenceScheduleBounds(t *testing.T) {
	for _, tc := range []struct {
		delays []*int64
		limit  *int64
		valid  bool
	}{
		{[]*int64{ms(0)}, nil, true},
		{[]*int64{ms(maxSequenceMS)}, ms(60000), true},
		{[]*int64{ms(maxSequenceMS), ms(1)}, nil, false},
		{[]*int64{ms(1)}, ms(60001), false},
		{[]*int64{nil}, nil, false},
	} {
		if _, _, err := sequenceSchedule(tc.delays, tc.limit); (err == nil) != tc.valid {
			t.Fatal("wrong schedule validation", err)
		}
	}
}
