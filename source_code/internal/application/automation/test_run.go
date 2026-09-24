package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"pbmt/internal/application/catalog"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/application/svpub"
	"pbmt/internal/domain/goose"
)

type TestExpectation struct {
	StreamID string `json:"stream_id"`
	SignalID string `json:"signal_id"`
	Edge     string `json:"edge"`
}
type TestRequest struct {
	StreamID        string          `json:"stream_id"`
	Revision        string          `json:"catalog_revision"`
	Steps           []SVStep        `json:"steps"`
	Expect          TestExpectation `json:"expect"`
	TimeoutMS       int64           `json:"timeout_ms"`
	MeasureFromStep int             `json:"measure_from_step,omitempty"`
	MaxLatenessMS   *int64          `json:"max_lateness_ms,omitempty"`
}
type TestID struct {
	ID string `json:"test_id"`
}
type TestStepResult struct {
	Step        int            `json:"step"`
	SentAt      time.Time      `json:"sent_at"`
	FromStartMS float64        `json:"from_start_ms"`
	Channels    []ChannelValue `json:"channels"`
}
type TestStatus struct {
	ID               string           `json:"test_id"`
	StreamID         string           `json:"stream_id"`
	CatalogRevision  string           `json:"catalog_revision"`
	Expect           TestExpectation  `json:"expect"`
	State            string           `json:"state"`
	Phase            string           `json:"phase"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       *time.Time       `json:"finished_at,omitempty"`
	TimeoutMS        int64            `json:"timeout_ms"`
	MeasureFromStep  int              `json:"measure_from_step"`
	Steps            []TestStepResult `json:"steps,omitempty"`
	StepsTotal       int              `json:"steps_total"`
	StepsApplied     int              `json:"steps_applied"`
	ResponseStep     int              `json:"response_step,omitempty"`
	ResponseAt       *time.Time       `json:"response_at,omitempty"`
	ElapsedMS        *float64         `json:"elapsed_ms,omitempty"`
	FromStartMS      *float64         `json:"from_start_ms,omitempty"`
	ResetConfirmed   bool             `json:"reset_confirmed"`
	ResetAt          *time.Time       `json:"reset_at,omitempty"`
	ResetFromStartMS *float64         `json:"reset_from_start_ms,omitempty"`
	Measurement      string           `json:"measurement"`
	Error            string           `json:"error,omitempty"`
}
type testRun struct {
	status TestStatus // guarded by API.mu; nested slices are immutable once published
	order  uint64
	cancel context.CancelFunc
	done   chan struct{}
}
type testPlan struct {
	request     TestRequest
	schedule    []sequenceStep
	waveforms   []svpub.Snapshot
	touched     map[int]bool
	output      catalog.Stream
	description *sclmodel.GooseStream
	input       catalog.Stream
	path        []int
	baseline    *goose.PDU
	watch       *goosesub.Watch
	agent       control.Agent
	measured    control.TestAgent
	limit       time.Duration
}

// RunTest arms the receiver before any stimulus. Only one test is allowed at a
// time: concurrent stimuli would make attribution of the response ambiguous.
func (a *API) RunTest(req TestRequest) (TestStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ready(); err != nil {
		return TestStatus{}, err
	}
	if err := a.streamIdle(req.StreamID); err != nil {
		return TestStatus{}, err
	}
	for _, r := range a.tests {
		if r.status.State == "running" {
			return TestStatus{}, errors.New("another test is running")
		}
	}
	measured, ok := a.agent.(control.TestAgent)
	if !ok {
		return TestStatus{}, errors.New("runtime does not support measured tests")
	}
	out, err := a.stream(req.StreamID, "sv", true)
	if err != nil {
		return TestStatus{}, err
	}
	in, err := a.stream(req.Expect.StreamID, "goose", false)
	if err != nil || !strings.HasPrefix(in.ID, "goose_sub/") {
		return TestStatus{}, errors.New("expect.stream_id must be a GOOSE input")
	}
	desc := a.descriptions[in.ID]
	path := booleanSignalPath(in.Signals, req.Expect.SignalID, nil)
	if desc == nil || len(path) == 0 {
		return TestStatus{}, errors.New("expect.signal_id must be a concrete ICD-described BOOLEAN input")
	}
	if req.Expect.Edge != "rising" && req.Expect.Edge != "falling" {
		return TestStatus{}, errors.New("expect.edge must be rising or falling")
	}
	if req.TimeoutMS < 1 || req.TimeoutMS > 60000 {
		return TestStatus{}, errors.New("timeout_ms must be between 1 and 60000")
	}
	if req.MeasureFromStep == 0 {
		req.MeasureFromStep = 1
	}
	if req.MeasureFromStep < 1 || req.MeasureFromStep > len(req.Steps) {
		return TestStatus{}, errors.New("measure_from_step must name a 1-based step")
	}
	delays := make([]*int64, len(req.Steps))
	for i, s := range req.Steps {
		delays[i] = s.AfterMS
	}
	schedule, limit, err := sequenceSchedule(delays, req.MaxLatenessMS)
	if err != nil {
		return TestStatus{}, err
	}
	if schedule[0].at != 0 || schedule[len(schedule)-1].at >= time.Duration(req.TimeoutMS)*time.Millisecond {
		return TestStatus{}, errors.New("first after_ms must be zero; all steps must be before timeout_ms")
	}
	current, status, err := a.agent.SVOutput(out.Name)
	if err != nil {
		return TestStatus{}, err
	}
	if status.State != control.StatusRunning {
		return TestStatus{}, errors.New("SV publisher is not running")
	}
	p := testPlan{request: req, schedule: schedule, touched: map[int]bool{}, output: out, input: in, description: desc, path: path, agent: a.agent, measured: measured, limit: time.Duration(limit) * time.Millisecond}
	for i, step := range req.Steps {
		next, err := a.prepareSV(SVSetRequest{req.StreamID, req.Revision, step.Changes}, out, current)
		if err != nil {
			return TestStatus{}, fmt.Errorf("step %d: %w", i+1, err)
		}
		if next.Settings == current.Settings {
			return TestStatus{}, fmt.Errorf("step %d does not change the waveform", i+1)
		}
		for _, change := range step.Changes {
			for ch, sig := range out.Signals {
				if change.SignalID == sig.ID {
					p.touched[ch] = true
				}
			}
		}
		p.waveforms = append(p.waveforms, next)
		current = next
	}
	p.watch, err = measured.ObserveGoose(in.Name)
	if err != nil {
		return TestStatus{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			p.watch.Cancel()
		}
	}()
	if len(p.watch.Initial) != 1 || p.watch.Initial[0].Stale || p.watch.Initial[0].PDU == nil {
		return TestStatus{}, errors.New("test requires one fresh, unambiguous GOOSE input")
	}
	p.baseline = p.watch.Initial[0].PDU
	value, err := p.inputValue(p.baseline)
	if err != nil {
		return TestStatus{}, err
	}
	if value == (req.Expect.Edge == "rising") {
		return TestStatus{}, errors.New("expected input already at target level; reset it before testing")
	}
	if p.watch.Initial[0].ObservedAt.IsZero() {
		return TestStatus{}, errors.New("GOOSE input has no application receive timestamp")
	}
	if a.tests == nil {
		a.tests = map[string]*testRun{}
	}
	if len(a.tests) >= 64 {
		var oldest *testRun
		for _, r := range a.tests {
			if r.status.State != "running" && (oldest == nil || r.order < oldest.order) {
				oldest = r
			}
		}
		if oldest == nil {
			return TestStatus{}, errors.New("test history full")
		}
		delete(a.tests, oldest.status.ID)
	}
	a.nextTest++
	ctx, cancel := context.WithCancel(context.Background())
	r := &testRun{order: a.nextTest, cancel: cancel, done: make(chan struct{}), status: TestStatus{
		ID: fmt.Sprintf("test-%d", a.nextTest), StreamID: out.ID, CatalogRevision: a.Catalog.Revision, Expect: req.Expect,
		State: "running", Phase: "armed", StartedAt: time.Now(), TimeoutMS: req.TimeoutMS, MeasureFromStep: req.MeasureFromStep,
		Steps: []TestStepResult{}, StepsTotal: len(p.waveforms), Measurement: "software: before successful SV WriteFrame to GOOSE capture-read return; monotonic interval; app-clock send dates; not NIC timestamps",
	}}
	a.tests[r.status.ID] = r
	accepted = true
	slog.Info("MCP test started", "test_id", r.status.ID, "stream_id", out.ID, "input_stream_id", in.ID, "steps", len(p.waveforms))
	go a.executeTest(ctx, r, p)
	return cloneTestStatus(r.status), nil
}

func booleanSignalPath(signals []catalog.Signal, id string, prefix []int) []int {
	for i, s := range signals {
		if s.IsTemplate {
			continue
		}
		path := append(append([]int(nil), prefix...), i)
		if s.ID == id && s.Type == "BOOLEAN" {
			return path
		}
		if s.Type == "STRUCTURE" {
			if found := booleanSignalPath(s.Children, id, path); found != nil {
				return found
			}
		}
	}
	return nil
}

func (p testPlan) inputValue(v *goose.PDU) (bool, error) {
	if v == nil {
		return false, errors.New("missing GOOSE data")
	}
	if err := p.description.ValidatePDU(v); err != nil {
		return false, fmt.Errorf("ICD mismatch: %w", err)
	}
	if v.Test || v.Simulation || v.NdsCom {
		return false, errors.New("test/simulation/uncommissioned GOOSE cannot establish a test response")
	}
	if p.baseline != nil && (v.SrcMAC.String() != p.baseline.SrcMAC.String() || v.DstMAC.String() != p.baseline.DstMAC.String() || v.AppID != p.baseline.AppID || v.GocbRef != p.baseline.GocbRef) {
		return false, errors.New("GOOSE input identity changed during test")
	}
	values := v.AllData
	var selected goose.DataValue
	for _, i := range p.path {
		if i >= len(values) {
			return false, errors.New("GOOSE structure changed")
		}
		selected = values[i]
		values = selected.Children
	}
	if selected.Type != goose.DataTypeBoolean {
		return false, errors.New("expected BOOLEAN input")
	}
	// Conservatively reject any nonzero Quality in this DataSet. An unknown
	// quality association must not turn invalid data into a successful result.
	if !zeroTestQuality(p.input.Signals, v.AllData) {
		return false, errors.New("nonzero GOOSE Quality during test")
	}
	return selected.Bool, nil
}

func zeroTestQuality(schema []catalog.Signal, values []goose.DataValue) bool {
	for i, sig := range schema {
		if i >= len(values) {
			return false
		}
		v := values[i]
		if sig.Type == "Quality" {
			for _, b := range v.Bytes {
				if b != 0 {
					return false
				}
			}
		}
		if sig.Type == "STRUCTURE" && !zeroTestQuality(sig.Children, v.Children) {
			return false
		}
		if sig.Type == "ARRAY" && len(sig.Children) == 1 {
			for _, child := range v.Children {
				if !zeroTestQuality(sig.Children, []goose.DataValue{child}) {
					return false
				}
			}
		}
	}
	return true
}

func (a *API) executeTest(ctx context.Context, r *testRun, p testPlan) {
	state, reason := "failed", "test did not complete"
	var current svpub.Snapshot
	var receipts []svpub.SendReceipt
	wrote := false
	defer func() {
		p.watch.Cancel()
		a.mu.Lock()
		r.status.Phase = "resetting"
		a.mu.Unlock()
		resetOK := !wrote
		var resetAt *time.Time
		var resetFromStart *float64
		if wrote {
			for ch := range p.touched {
				current.Settings[ch].RMS = 0
			}
			ch, err := p.measured.ApplySVTracked(p.output.Name, current.Settings, current.Simulation, current.Synch)
			if err == nil {
				timer := time.NewTimer(time.Second)
				select {
				case receipt := <-ch:
					err = receipt.Err
					if err == nil && !receipt.At.IsZero() {
						resetOK = true
						at := receipt.AppTime
						resetAt = &at
						if len(receipts) > 0 {
							resetFromStart = finite(float64(receipt.At.Sub(receipts[0].At)) / float64(time.Millisecond))
						}
					} else if err == nil {
						err = errors.New("missing reset send timestamp")
					}
				case <-timer.C:
					err = errors.New("reset frame was not confirmed within 1s")
				}
				timer.Stop()
			}
			if err != nil {
				// Do not leave a failed reset transmitting. This exceptional path
				// revokes the lease and stops BOTH publishers, never PTP.
				a.Revoke()
				a.mu.Lock()
				for _, sequence := range a.sequences {
					a.finishSequence(sequence, "cancelled", "test output reset failed")
				}
				a.mu.Unlock()
				stopErr := p.agent.Close()
				state = "failed"
				reason = fmt.Sprintf("%s; output reset failed: %v; publishers stopped (error: %v)", reason, err, stopErr)
			}
		}
		now := time.Now()
		a.mu.Lock()
		r.status.State = state
		r.status.Phase = "finished"
		r.status.Error = reason
		r.status.ResetConfirmed = resetOK
		r.status.ResetAt = resetAt
		r.status.ResetFromStartMS = resetFromStart
		r.status.FinishedAt = &now
		args := []any{"test_id", r.status.ID, "state", state, "reset_confirmed", resetOK, "error", reason}
		if state == "failed" {
			slog.Error("MCP test finished", args...)
		} else {
			slog.Info("MCP test finished", args...)
		}
		r.cancel()
		close(r.done)
		a.mu.Unlock()
	}()
	lastInput := p.watch.Initial[0].ObservedAt
	inputDeadline := lastInput.Add(time.Duration(p.baseline.TimeAllowedToLiveMs) * time.Millisecond)
	previous, _ := p.inputValue(p.baseline)
	var pending <-chan svpub.SendReceipt
	next := 0
	deadline := time.Now().Add(time.Duration(p.request.TimeoutMS) * time.Millisecond)
	sendDeadline := time.Now().Add(time.Second)
	check := time.NewTicker(50 * time.Millisecond)
	defer check.Stop()
	handle := func(event goosesub.Observation) bool {
		if event.Restart {
			reason = "GOOSE publisher restarted during test"
			return true
		}
		if event.At.IsZero() || event.At.Before(lastInput) {
			reason = "missing or reordered application receive timestamp"
			return true
		}
		value, err := p.inputValue(event.PDU)
		if err != nil {
			reason = err.Error()
			return true
		}
		if event.At.After(inputDeadline) {
			reason = "GOOSE input expired before response"
			return true
		}
		lastInput = event.At
		inputDeadline = event.At.Add(time.Duration(event.PDU.TimeAllowedToLiveMs) * time.Millisecond)
		if value != previous && value == (p.request.Expect.Edge == "rising") {
			if len(receipts) == 0 || event.At.Before(receipts[0].At) {
				reason = "input reached target before stimulus"
				return true
			}
			if event.At.After(deadline) {
				state, reason = "timeout", ""
				return true
			}
			index := p.request.MeasureFromStep - 1
			if index >= len(receipts) || event.At.Before(receipts[index].At) {
				reason = "response before measure_from_step"
				return true
			}
			responseStep := 1
			for i, tx := range receipts {
				if !tx.At.After(event.At) {
					responseStep = i + 1
				}
			}
			elapsed := float64(event.At.Sub(receipts[index].At)) / float64(time.Millisecond)
			fromStart := float64(event.At.Sub(receipts[0].At)) / float64(time.Millisecond)
			at := receipts[0].AppTime.Add(event.At.Sub(receipts[0].At))
			a.mu.Lock()
			r.status.ResponseStep = responseStep
			r.status.ResponseAt = &at
			r.status.ElapsedMS = &elapsed
			r.status.FromStartMS = &fromStart
			a.mu.Unlock()
			state, reason = "triggered", ""
			return true
		}
		previous = value
		return false
	}
	for {
		if ctx.Err() != nil {
			state, reason = "cancelled", ""
			return
		}
		select {
		case <-p.watch.Failed:
			reason = "GOOSE observation overflow; measurement invalid"
			return
		case <-p.watch.Closed:
			reason = "GOOSE receiver stopped"
			return
		default:
		}
		// Prefer queued input over another stimulus or a timeout. Bound the
		// drain by queue capacity so a busy input cannot starve cancellation.
		if pending == nil {
			for n := len(p.watch.Frames); n > 0; n-- {
				if handle(<-p.watch.Frames) {
					return
				}
			}
		}
		if !time.Now().Before(deadline) {
			state, reason = "timeout", ""
			return
		}
		if !time.Now().Before(inputDeadline) && pending == nil {
			reason = "GOOSE input expired"
			return
		}
		if pending == nil && next < len(p.waveforms) && (next == 0 || !time.Now().Before(receipts[0].At.Add(p.schedule[next].at))) {
			if next > 0 && time.Since(receipts[0].At.Add(p.schedule[next].at)) > p.limit {
				reason = "test step exceeded max_lateness_ms"
				return
			}
			a.mu.Lock()
			err := a.ready()
			a.mu.Unlock()
			if err != nil {
				reason = err.Error()
				return
			}
			if ctx.Err() != nil || a.revoked.Load() {
				state, reason = "cancelled", ""
				return
			}
			current = p.waveforms[next]
			wrote = true // reset even if application reports a partial failure
			pending, err = p.measured.ApplySVTracked(p.output.Name, current.Settings, current.Simulation, current.Synch)
			if err != nil {
				reason = err.Error()
				return
			}
			sendDeadline = time.Now().Add(time.Second)
		}
		wake := deadline
		var frames <-chan goosesub.Observation
		if pending != nil {
			if sendDeadline.Before(wake) {
				wake = sendDeadline
			}
		} else {
			frames = p.watch.Frames
			if inputDeadline.Before(wake) {
				wake = inputDeadline
			}
			if next < len(p.waveforms) {
				due := receipts[0].At.Add(p.schedule[next].at)
				if due.Before(wake) {
					wake = due
				}
			}
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			state, reason = "cancelled", ""
			return
		case <-p.watch.Failed:
			timer.Stop()
			reason = "GOOSE observation overflow; measurement invalid"
			return
		case <-p.watch.Closed:
			timer.Stop()
			reason = "GOOSE receiver stopped"
			return
		case receipt := <-pending:
			timer.Stop()
			if receipt.Err != nil || receipt.At.IsZero() {
				reason = fmt.Sprintf("SV send failed or timestamp missing: %v", receipt.Err)
				return
			}
			receipts = append(receipts, receipt)
			pending = nil
			next++
			if next == 1 {
				deadline = receipt.At.Add(time.Duration(p.request.TimeoutMS) * time.Millisecond)
			}
			if next > 1 && receipt.At.Sub(receipts[0].At.Add(p.schedule[next-1].at)) > p.limit {
				reason = "SV send exceeded max_lateness_ms"
				return
			}
			step := TestStepResult{Step: next, SentAt: receipt.AppTime, FromStartMS: float64(receipt.At.Sub(receipts[0].At)) / float64(time.Millisecond)}
			for i, sig := range p.output.Signals {
				setting := current.Settings[i]
				step.Channels = append(step.Channels, ChannelValue{ID: sig.ID, Name: sig.Name, Unit: sig.Unit, RMS: finite(setting.RMS), Phase: finite(setting.PhaseDeg), Frequency: finite(setting.Frequency), Quality: uint32(setting.Quality)})
			}
			a.mu.Lock()
			r.status.Phase = "waiting"
			r.status.Steps = append(r.status.Steps, step)
			r.status.StepsApplied = next
			a.mu.Unlock()
		case event := <-frames:
			timer.Stop()
			if handle(event) {
				return
			}
		case <-check.C:
			timer.Stop()
			a.mu.Lock()
			err := a.ready()
			a.mu.Unlock()
			if err != nil {
				reason = err.Error()
				return
			}
			states, err := p.agent.Statuses()
			if err != nil {
				reason = err.Error()
				return
			}
			for _, s := range states {
				if (s.ID == control.ModuleSVPub || s.ID == control.ModuleGooseSub) && s.State != control.StatusRunning {
					reason = "test module stopped or failed"
					return
				}
			}
		case <-timer.C:
			if pending != nil && !time.Now().Before(sendDeadline) {
				reason = "SV frame not confirmed within 1s"
				return
			}
			// The next iteration drains captured input before checking deadlines.
		}
	}
}

func cloneTestStatus(s TestStatus) TestStatus {
	s.Steps = append([]TestStepResult{}, s.Steps...)
	return s
}
func (a *API) GetTest(req TestID) (TestStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.tests[req.ID]
	if r == nil {
		return TestStatus{}, errors.New("unknown or expired test_id")
	}
	return cloneTestStatus(r.status), nil
}
func (a *API) CancelTest(req TestID) (TestStatus, error) {
	a.mu.Lock()
	r := a.tests[req.ID]
	if r == nil {
		a.mu.Unlock()
		return TestStatus{}, errors.New("unknown or expired test_id")
	}
	r.cancel()
	a.mu.Unlock()
	<-r.done
	return a.GetTest(req)
}
func (a *API) testStatuses() []TestStatus {
	runs := make([]*testRun, 0, len(a.tests))
	for _, r := range a.tests {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].order < runs[j].order })
	out := make([]TestStatus, 0, len(runs))
	for _, r := range runs {
		summary := r.status
		summary.Steps = nil // large waveform histories belong to get_test, not status polling
		out = append(out, summary)
	}
	return out
}
