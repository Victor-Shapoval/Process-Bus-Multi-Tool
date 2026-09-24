package automation

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"pbmt/internal/application/catalog"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/application/svpub"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

type measuredRuntime struct {
	control.Agent
	mu                       sync.Mutex
	output                   svpub.Snapshot
	watch                    *goosesub.Watch
	frames                   chan goosesub.Observation
	failed, closed           chan struct{}
	pdu                      *goose.PDU
	writes, closes, releases int
	resetError, sendError    bool
	delayFirst               bool
	firstReceipt             chan svpub.SendReceipt
	cancelled                bool
}

func (m *measuredRuntime) SVOutput(string) (svpub.Snapshot, control.ModuleStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.output, control.ModuleStatus{ID: control.ModuleSVPub, State: control.StatusRunning}, nil
}
func (m *measuredRuntime) ObserveGoose(string) (*goosesub.Watch, error) { return m.watch, nil }
func (m *measuredRuntime) Statuses() ([]control.ModuleStatus, error) {
	return []control.ModuleStatus{{ID: control.ModuleSVPub, State: control.StatusRunning}, {ID: control.ModuleGooseSub, State: control.StatusRunning}}, nil
}
func (m *measuredRuntime) ApplySVTracked(_ string, settings [sv.NumChannels]svpub.ChannelSetting, sim bool, synch sv.SmpSynch) (<-chan svpub.SendReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.resetError && m.writes > 1 {
		return nil, errors.New("reset failed")
	}
	m.output.Settings = settings
	m.output.Simulation = sim
	m.output.Synch = synch
	ch := make(chan svpub.SendReceipt, 1)
	if m.delayFirst && m.writes == 1 {
		m.firstReceipt = ch
		return ch, nil
	}
	r := svpub.SendReceipt{At: time.Now(), AppTime: time.Now().Add(time.Hour)}
	if m.sendError && m.writes == 1 {
		r.Err = errors.New("send failed")
	}
	ch <- r
	return ch, nil
}
func (m *measuredRuntime) Close() error   { m.mu.Lock(); defer m.mu.Unlock(); m.closes++; return nil }
func (m *measuredRuntime) Release() error { m.mu.Lock(); defer m.mu.Unlock(); m.releases++; return nil }
func (m *measuredRuntime) emit(value bool, at time.Time) {
	p := goose.ClonePDU(m.pdu)
	p.AllData[0].Bool = value
	m.frames <- goosesub.Observation{PDU: p, At: at}
}

func measuredAPI(t *testing.T) (*API, *measuredRuntime, TestRequest) {
	a, old := testAPI(t)
	out := streamFor(t, a, "sv_pub")
	in := streamFor(t, a, "goose_sub")
	in.Signals = []catalog.Signal{{ID: in.ID + "/entry/0", Name: "Trip", Type: "BOOLEAN"}}
	a.streams[in.ID] = in
	desc := &sclmodel.GooseStream{GocbRef: "IED/LLN0$GO$Trip", DatSet: "IED/LLN0$TripData", DstMAC: "01:0c:cd:01:00:01", AppID: 1, ConfRev: 2, Entries: []*sclmodel.Type{{Name: "Trip", Kind: "BOOLEAN"}}}
	a.descriptions[in.ID] = desc
	mac, _ := net.ParseMAC(desc.DstMAC)
	pdu := &goose.PDU{GocbRef: desc.GocbRef, DatSet: desc.DatSet, DstMAC: mac, AppID: 1, ConfRev: 2, NumDatSetEntries: 1, TimeAllowedToLiveMs: 60000, AllData: []goose.DataValue{{Type: goose.DataTypeBoolean}}}
	m := &measuredRuntime{output: old.sv, pdu: pdu, frames: make(chan goosesub.Observation, 256), failed: make(chan struct{}), closed: make(chan struct{})}
	m.watch = &goosesub.Watch{Initial: []goosesub.Snapshot{{PDU: goose.ClonePDU(pdu), ReceivedAt: time.Now(), ObservedAt: time.Now()}}, Frames: m.frames, Failed: m.failed, Closed: m.closed, Cancel: func() { m.mu.Lock(); m.cancelled = true; m.mu.Unlock() }}
	a.Attach(m)
	req := TestRequest{StreamID: out.ID, Revision: a.Catalog.Revision, Steps: []SVStep{{AfterMS: ms(0), Changes: []SVChange{{SignalID: out.Signals[0].ID, RMS: finite(1200)}}}}, Expect: TestExpectation{StreamID: in.ID, SignalID: in.Signals[0].ID, Edge: "rising"}, TimeoutMS: 500}
	return a, m, req
}
func testState(t *testing.T, a *API, id, state string) TestStatus {
	t.Helper()
	s, err := a.GetTest(TestID{id})
	if err != nil || s.State != state {
		t.Fatalf("got %+v error %v; want %s", s, err, state)
	}
	return s
}

func TestMeasuredShortPulseUsesSendReceiptAndResets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, m, req := measuredAPI(t)
		defer a.Release()
		m.delayFirst = true
		before := m.output
		r, err := a.RunTest(req)
		if err != nil {
			t.Fatal(err)
		}
		// The send boundary may be much later than command application.
		synctest.Wait()
		time.Sleep(20 * time.Millisecond)
		tx := time.Now()
		m.firstReceipt <- svpub.SendReceipt{At: tx, AppTime: tx.Add(time.Hour)}
		synctest.Wait()
		time.Sleep(6 * time.Millisecond)
		m.emit(true, time.Now())
		m.emit(false, time.Now().Add(time.Microsecond))
		synctest.Wait()
		s := testState(t, a, r.ID, "triggered")
		if s.ElapsedMS == nil || *s.ElapsedMS != 6 || s.ResponseStep != 1 || !s.ResetConfirmed || len(s.Steps) != 1 {
			t.Fatalf("bad result %+v", s)
		}
		if !s.ResponseAt.Equal(tx.Add(time.Hour + 6*time.Millisecond)) {
			t.Fatal("incorrect app-clock report date")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.output.Settings[0].RMS != 0 || m.output.Settings[1] != before.Settings[1] || m.output.Simulation != before.Simulation || m.writes != 2 || !m.cancelled {
			t.Fatal("reset did not preserve unrelated settings")
		}
	})
}

func TestMeasuredStepsAndOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, m, req := measuredAPI(t)
		defer a.Release()
		req.Steps = append(req.Steps, SVStep{AfterMS: ms(100), Changes: []SVChange{{SignalID: req.Steps[0].Changes[0].SignalID, RMS: finite(1500)}}})
		req.MeasureFromStep = 2
		r, err := a.RunTest(req)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if _, err := a.SVSet(SVSetRequest{req.StreamID, req.Revision, req.Steps[0].Changes}); err == nil {
			t.Fatal("test allowed competing write")
		}
		if _, err := a.TimedSVSet(TimedSVRequest{StreamID: req.StreamID, Revision: req.Revision, Steps: req.Steps}); err == nil {
			t.Fatal("test allowed competing sequence")
		}
		// Caller mutation must not change accepted plan settings.
		*req.Steps[1].Changes[0].RMS = 1700
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		time.Sleep(50 * time.Millisecond)
		m.emit(true, time.Now())
		synctest.Wait()
		s := testState(t, a, r.ID, "triggered")
		if *s.ElapsedMS != 50 || *s.FromStartMS != 150 || s.ResponseStep != 2 || len(s.Steps) != 2 || *s.Steps[1].Channels[0].RMS != 1500 {
			t.Fatalf("bad step result %+v", s)
		}
	})
}

func TestMeasuredTerminationResets(t *testing.T) {
	for _, ending := range []string{"timeout", "cancel", "release", "close", "overflow", "receiver_closed", "schema", "send_error", "reset_error", "premature", "falling", "missing_stamp"} {
		t.Run(ending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, m, req := measuredAPI(t)
				if ending == "send_error" {
					m.sendError = true
				}
				if ending == "reset_error" {
					m.resetError = true
				}
				if ending == "falling" {
					req.Expect.Edge = "falling"
					m.watch.Initial[0].PDU.AllData[0].Bool = true
				}
				r, err := a.RunTest(req)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				expected := "failed"
				switch ending {
				case "timeout":
					time.Sleep(500 * time.Millisecond)
					expected = "timeout"
				case "cancel":
					_, err = a.CancelTest(TestID{r.ID})
					expected = "cancelled"
				case "release":
					err = a.Release()
					expected = "cancelled"
				case "close":
					err = a.Close()
					expected = "cancelled"
				case "overflow":
					close(m.failed)
				case "receiver_closed":
					close(m.closed)
				case "schema":
					m.pdu.ConfRev++
					m.emit(true, time.Now())
				case "reset_error":
					m.emit(true, time.Now())
				case "premature":
					m.emit(true, time.Now().Add(-time.Nanosecond))
				case "falling":
					time.Sleep(10 * time.Millisecond)
					m.emit(false, time.Now())
					expected = "triggered"
				case "missing_stamp":
					m.emit(true, time.Time{})
				}
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				s := testState(t, a, r.ID, expected)
				if ending == "reset_error" {
					if s.ResetConfirmed || !strings.Contains(s.Error, "reset failed") || m.closes != 1 {
						t.Fatal("reset failure not surfaced", s)
					}
				} else if !s.ResetConfirmed || m.output.Settings[0].RMS != 0 {
					t.Fatal("output not reset", s)
				}
				if ending == "release" && (m.releases != 1 || m.closes != 0) {
					t.Fatal("release stopped publishers")
				}
				if ending == "close" && m.closes != 1 {
					t.Fatal("disconnect did not stop publishers")
				}
				if _, err := a.CancelTest(TestID{r.ID}); err != nil {
					t.Fatal(err)
				}
				_ = a.Release()
			})
		})
	}
}

func TestMeasuredValidationBeforeOutput(t *testing.T) {
	for _, kind := range []string{"revision", "target_high", "stale", "ambiguous", "no_schema", "bad_id", "bad_edge", "zero_timeout", "bad_step", "no_change", "quality_flag", "missing_receive_time"} {
		t.Run(kind, func(t *testing.T) {
			a, m, req := measuredAPI(t)
			switch kind {
			case "revision":
				req.Revision = "bad"
			case "target_high":
				m.watch.Initial[0].PDU.AllData[0].Bool = true
			case "stale":
				m.watch.Initial[0].Stale = true
			case "ambiguous":
				m.watch.Initial = append(m.watch.Initial, m.watch.Initial[0])
			case "no_schema":
				delete(a.descriptions, req.Expect.StreamID)
			case "bad_id":
				req.Expect.SignalID = "bad"
			case "bad_edge":
				req.Expect.Edge = "any"
			case "zero_timeout":
				req.TimeoutMS = 0
			case "bad_step":
				req.MeasureFromStep = 2
			case "no_change":
				req.Steps[0].Changes[0].RMS = finite(m.output.Settings[0].RMS)
			case "quality_flag":
				m.watch.Initial[0].PDU.Test = true
			case "missing_receive_time":
				m.watch.Initial[0].ObservedAt = time.Time{}
			}
			if _, err := a.RunTest(req); err == nil {
				t.Fatal("invalid test accepted")
			}
			if m.writes != 0 {
				t.Fatal("validation changed output")
			}
		})
	}
}

func TestMeasuredMissingSendInputLossAndSourceChange(t *testing.T) {
	for _, kind := range []string{"missing_send", "input_expired", "restart", "changed_sources", "early_origin", "late_edge"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, m, req := measuredAPI(t)
				defer a.Release()
				if kind == "missing_send" {
					m.delayFirst = true
					req.TimeoutMS = 2000
				}
				if kind == "input_expired" {
					m.watch.Initial[0].PDU.TimeAllowedToLiveMs = 10
				}
				if kind == "early_origin" {
					req.Steps = append(req.Steps, SVStep{AfterMS: ms(100), Changes: []SVChange{{SignalID: req.Steps[0].Changes[0].SignalID, RMS: finite(1500)}}})
					req.MeasureFromStep = 2
				}
				r, err := a.RunTest(req)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				expected := "failed"
				switch kind {
				case "missing_send":
					time.Sleep(time.Second)
				case "input_expired":
					time.Sleep(11 * time.Millisecond)
				case "restart":
					m.frames <- goosesub.Observation{PDU: goose.ClonePDU(m.pdu), At: time.Now(), Restart: true}
				case "changed_sources":
					if err := os.WriteFile(filepath.Join(a.root, "cfg", "sv_pub.yaml"), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
					time.Sleep(50 * time.Millisecond)
				case "early_origin":
					time.Sleep(10 * time.Millisecond)
					m.emit(true, time.Now())
				case "late_edge":
					time.Sleep(500 * time.Millisecond)
					synctest.Wait()
					m.emit(true, time.Now())
					expected = "timeout"
				}
				synctest.Wait()
				s := testState(t, a, r.ID, expected)
				if !s.ResetConfirmed || m.output.Settings[0].RMS != 0 || s.ElapsedMS != nil {
					t.Fatal("failed test retained output or reported a measurement", s)
				}
			})
		})
	}
}
