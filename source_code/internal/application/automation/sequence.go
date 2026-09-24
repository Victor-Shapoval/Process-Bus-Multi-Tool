package automation

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"pbmt/internal/application/control"
)

const (
	maxSequenceSteps = 1000
	maxSequences     = 64
	maxSequenceMS    = int64(24 * 60 * 60 * 1000)
)

type GooseStep struct {
	AfterMS *int64        `json:"after_ms"`
	Changes []GooseChange `json:"changes"`
}
type SVStep struct {
	AfterMS *int64     `json:"after_ms"`
	Changes []SVChange `json:"changes"`
}
type TimedGooseRequest struct {
	StreamID      string      `json:"stream_id"`
	Revision      string      `json:"catalog_revision"`
	Steps         []GooseStep `json:"steps"`
	MaxLatenessMS *int64      `json:"max_lateness_ms,omitempty"`
}
type TimedSVRequest struct {
	StreamID      string   `json:"stream_id"`
	Revision      string   `json:"catalog_revision"`
	Steps         []SVStep `json:"steps"`
	MaxLatenessMS *int64   `json:"max_lateness_ms,omitempty"`
}
type SequenceRequest struct {
	ID string `json:"sequence_id"`
}
type SequenceStatus struct {
	ID              string     `json:"sequence_id"`
	Tool            string     `json:"tool"`
	StreamID        string     `json:"stream_id"`
	CatalogRevision string     `json:"catalog_revision"`
	State           string     `json:"state"`
	StepsTotal      int        `json:"steps_total"`
	StepsApplied    int        `json:"steps_applied"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	LastAppliedAt   *time.Time `json:"last_applied_at,omitempty"`
	MaxLatenessMS   float64    `json:"max_observed_lateness_ms"`
	LatenessLimitMS int64      `json:"max_lateness_ms"`
	Error           string     `json:"error,omitempty"`
}

type sequenceRun struct {
	status SequenceStatus
	order  uint64
	done   chan struct{}
}
type sequenceStep struct {
	at    time.Duration
	apply func() error
}

// All sequence state and publisher writes are serialized by API.mu. Waiting
// never holds the lock. Timers use the monotonic portion of time.Time, not PTP
// or wall-clock time, and every deadline is relative to the same start instant.
func (a *API) TimedGooseSet(req TimedGooseRequest) (SequenceStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var empty SequenceStatus
	if err := a.ready(); err != nil {
		return empty, err
	}
	if err := a.streamIdle(req.StreamID); err != nil {
		return empty, err
	}
	s, err := a.stream(req.StreamID, "goose", true)
	if err != nil {
		return empty, err
	}
	delays := make([]*int64, len(req.Steps))
	for i, step := range req.Steps {
		delays[i] = step.AfterMS
	}
	steps, limit, err := sequenceSchedule(delays, req.MaxLatenessMS)
	if err != nil {
		return empty, err
	}
	current, status, err := a.agent.GooseOutput(s.Name)
	if err != nil {
		return empty, err
	}
	if status.State != control.StatusRunning {
		return empty, errors.New("GOOSE publisher is not running")
	}
	for i, step := range req.Steps {
		current, err = a.prepareGoose(GooseSetRequest{req.StreamID, req.Revision, step.Changes}, s, current)
		if err != nil {
			return empty, fmt.Errorf("step %d: %w", i+1, err)
		}
		// Each step owns a full validated snapshot; subsequent preflight steps
		// clone it. Caller-owned changes cannot mutate an accepted sequence.
		next := current
		steps[i].apply = func() error {
			_, state, err := a.agent.GooseOutput(s.Name)
			if err != nil {
				return err
			}
			if state.State != control.StatusRunning {
				return errors.New("GOOSE publisher is not running")
			}
			_, err = a.agent.ApplyGoose(s.Name, next.Data, next.Test, next.Simulation)
			return err
		}
	}
	return a.startSequence("t_set_goose", req.StreamID, steps, limit)
}

func (a *API) TimedSVSet(req TimedSVRequest) (SequenceStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var empty SequenceStatus
	if err := a.ready(); err != nil {
		return empty, err
	}
	if err := a.streamIdle(req.StreamID); err != nil {
		return empty, err
	}
	s, err := a.stream(req.StreamID, "sv", true)
	if err != nil {
		return empty, err
	}
	delays := make([]*int64, len(req.Steps))
	for i, step := range req.Steps {
		delays[i] = step.AfterMS
	}
	steps, limit, err := sequenceSchedule(delays, req.MaxLatenessMS)
	if err != nil {
		return empty, err
	}
	current, status, err := a.agent.SVOutput(s.Name)
	if err != nil {
		return empty, err
	}
	if status.State != control.StatusRunning {
		return empty, errors.New("SV publisher is not running")
	}
	for i, step := range req.Steps {
		current, err = a.prepareSV(SVSetRequest{req.StreamID, req.Revision, step.Changes}, s, current)
		if err != nil {
			return empty, fmt.Errorf("step %d: %w", i+1, err)
		}
		next := current
		steps[i].apply = func() error {
			v, state, err := a.agent.SVOutput(s.Name)
			if err != nil {
				return err
			}
			if state.State != control.StatusRunning || !v.Ready {
				return errors.New("SV publisher is not running or waveform is not ready")
			}
			return a.agent.ApplySV(s.Name, next.Settings, next.Simulation, next.Synch)
		}
	}
	return a.startSequence("t_set_sv", req.StreamID, steps, limit)
}

func sequenceSchedule(delays []*int64, requestedLimit *int64) ([]sequenceStep, int64, error) {
	limit := int64(100)
	if requestedLimit != nil {
		limit = *requestedLimit
	}
	if limit < 1 || limit > 60000 {
		return nil, 0, errors.New("max_lateness_ms must be between 1 and 60000")
	}
	if len(delays) == 0 || len(delays) > maxSequenceSteps {
		return nil, 0, errors.New("steps must contain 1 to 1000 entries")
	}
	steps := make([]sequenceStep, len(delays))
	var total int64
	for i, delay := range delays {
		if delay == nil || *delay < 0 || (i > 0 && *delay == 0) || *delay > maxSequenceMS-total {
			return nil, 0, fmt.Errorf("step %d: after_ms is required, must be positive (first step may be zero), and total duration must not exceed 24 hours", i+1)
		}
		total += *delay
		steps[i].at = time.Duration(total) * time.Millisecond
	}
	return steps, limit, nil
}

func (a *API) streamIdle(id string) error {
	for _, run := range a.tests {
		if run.status.StreamID == id && run.status.State == "running" {
			return fmt.Errorf("stream is controlled by %s; cancel_test before writing", run.status.ID)
		}
	}
	for _, run := range a.sequences {
		if run.status.StreamID == id && run.status.State == "running" {
			return fmt.Errorf("stream is controlled by %s; cancel_sequence before writing", run.status.ID)
		}
	}
	return nil
}

func (a *API) startSequence(tool, streamID string, steps []sequenceStep, limit int64) (SequenceStatus, error) {
	if err := a.ready(); err != nil {
		return SequenceStatus{}, err
	}
	if a.sequences == nil {
		a.sequences = make(map[string]*sequenceRun)
	}
	if len(a.sequences) >= maxSequences {
		var oldest *sequenceRun
		for _, run := range a.sequences {
			if run.status.State != "running" && (oldest == nil || run.order < oldest.order) {
				oldest = run
			}
		}
		if oldest == nil {
			return SequenceStatus{}, errors.New("too many active sequences")
		}
		delete(a.sequences, oldest.status.ID)
	}
	a.nextSequence++
	run := &sequenceRun{order: a.nextSequence, done: make(chan struct{}), status: SequenceStatus{
		ID: fmt.Sprintf("sequence-%d", a.nextSequence), Tool: tool, StreamID: streamID,
		CatalogRevision: a.Catalog.Revision, State: "running", StepsTotal: len(steps),
		StartedAt: time.Now(), LatenessLimitMS: limit,
	}}
	a.sequences[run.status.ID] = run
	slog.Info("MCP sequence started", "sequence_id", run.status.ID, "tool", tool, "stream_id", streamID, "steps", len(steps))
	go a.executeSequence(run, steps)
	return run.status, nil
}

func (a *API) executeSequence(run *sequenceRun, steps []sequenceStep) {
	// StartedAt is immutable and retains its monotonic clock component.
	for i, step := range steps {
		deadline := run.status.StartedAt.Add(step.at)
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-run.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		a.mu.Lock()
		if run.status.State != "running" {
			a.mu.Unlock()
			return
		}
		err := a.ready()
		lateness := time.Since(deadline)
		if err == nil && lateness > time.Duration(run.status.LatenessLimitMS)*time.Millisecond {
			err = errors.New("step deadline exceeded max_lateness_ms; remaining steps were not applied")
		}
		// Do not send a burst of obsolete steps after a scheduler stall.
		if err == nil && i+1 < len(steps) && !time.Now().Before(run.status.StartedAt.Add(steps[i+1].at)) {
			err = errors.New("next step deadline already passed; remaining steps were not applied")
		}
		if err == nil {
			err = step.apply()
			if err == nil {
				now := time.Now()
				run.status.StepsApplied++
				run.status.LastAppliedAt = &now
				lateness = now.Sub(deadline)
				if lateness > time.Duration(run.status.LatenessLimitMS)*time.Millisecond {
					err = errors.New("step application exceeded max_lateness_ms; remaining steps were not applied")
				}
			}
		}
		if ms := float64(lateness) / float64(time.Millisecond); ms > run.status.MaxLatenessMS {
			run.status.MaxLatenessMS = ms
		}
		if err != nil {
			a.finishSequence(run, "failed", fmt.Sprintf("step %d: %v", i+1, err))
		} else if i+1 == len(steps) {
			a.finishSequence(run, "completed", "")
		}
		finished := run.status.State != "running"
		a.mu.Unlock()
		if finished {
			return
		}
	}
}

func (a *API) finishSequence(run *sequenceRun, state, reason string) {
	if run.status.State != "running" {
		return
	}
	now := time.Now()
	run.status.State, run.status.Error, run.status.FinishedAt = state, reason, &now
	close(run.done)
	args := []any{"sequence_id", run.status.ID, "stream_id", run.status.StreamID, "state", state, "steps_applied", run.status.StepsApplied, "error", reason}
	if state == "failed" {
		slog.Warn("MCP sequence finished", args...)
	} else {
		slog.Info("MCP sequence finished", args...)
	}
}

func (a *API) GetSequence(req SequenceRequest) (SequenceStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sequenceStatus(req)
}

func (a *API) sequenceStatus(req SequenceRequest) (SequenceStatus, error) {
	if a.agent == nil || a.revoked.Load() {
		return SequenceStatus{}, errors.New("MCP control is not active")
	}
	run := a.sequences[req.ID]
	if run == nil {
		return SequenceStatus{}, errors.New("unknown or expired sequence_id")
	}
	return run.status, nil
}

// Cancel waits for any in-flight write, then fences all later steps. It leaves
// the last applied values in place, without stopping publishers or rolling back.
func (a *API) CancelSequence(req SequenceRequest) (SequenceStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.sequenceStatus(req); err != nil {
		return SequenceStatus{}, err
	}
	run := a.sequences[req.ID]
	a.finishSequence(run, "cancelled", "")
	return run.status, nil
}

func (a *API) sequenceStatuses() []SequenceStatus {
	runs := make([]*sequenceRun, 0, len(a.sequences))
	for _, run := range a.sequences {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].order < runs[j].order })
	statuses := make([]SequenceStatus, 0, len(runs))
	for _, run := range runs {
		statuses = append(statuses, run.status)
	}
	return statuses
}
