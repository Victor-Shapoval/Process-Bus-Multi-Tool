// Package goosesub provides the GOOSE message reception service (IEC 61850-8-1).
// It orchestrates Ethernet frame capture, PDU decoding, subscription filtering,
// protocol checks, and domain event publication.
package goosesub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

// watchdogTick is the interval between stale-stream checks.
const watchdogTick = 250 * time.Millisecond

// Service receives frames from ethernet.Source, decodes GOOSE PDUs,
// matches subscriptions, and publishes events to EventSink.
type Service struct {
	source ethernet.Source
	subs   []*goose.Subscription
	sink   EventSink
	log    *slog.Logger

	mu      sync.Mutex
	streams map[streamKey]*streamState
	watches map[*Watch]watchChannels
}

// streamKey identifies a GOOSE stream by destination MAC, AppID, and gocbRef.
type streamKey struct {
	dstMAC  string
	appID   uint16
	gocbRef string
}

// streamState holds stream state for deduplication, diagnostics, and the watchdog.
type streamState struct {
	sub            *goose.Subscription
	lastStNum      uint32
	lastSqNum      uint32
	lastConfRev    uint32
	lastSeenAt     time.Time
	lastObservedAt time.Time
	talMs          uint32
	lastPDU        *goose.PDU
	seen           bool
	lost           bool
}

// New creates a Service. Source must already be open.
// If sink is nil, events are discarded by a no-op sink.
func New(source ethernet.Source, subs []*goose.Subscription, sink EventSink, log *slog.Logger) *Service {
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

// Run reads frames from source until the channel closes or ctx is canceled.
// It is a blocking call and should run in a separate goroutine.
func (s *Service) Run(ctx context.Context) error {
	defer s.closeWatches()
	if len(s.subs) == 0 {
		return errors.New("goosesub: no subscriptions configured")
	}
	s.log.Debug("goosesub started", "subscriptions", len(s.subs))
	defer s.log.Debug("goosesub stopped")

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

// process decodes a frame and matches it against subscriptions.
func (s *Service) process(frame ethernet.Frame) {
	pdu, err := goose.Decode(frame.Data)
	if err != nil {
		// The frame is not GOOSE or is malformed; skip it.
		return
	}
	for _, sub := range s.subs {
		if !sub.Matches(pdu) {
			continue
		}
		s.handleMatch(sub, pdu, frame)
		return // first matching subscription
	}
}

// handleMatch performs deduplication and diagnostics, then publishes events.
func (s *Service) handleMatch(sub *goose.Subscription, pdu *goose.PDU, frame ethernet.Frame) {
	receivedAt := frame.Timestamp
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	key := streamKey{
		dstMAC:  pdu.DstMAC.String(),
		appID:   pdu.AppID,
		gocbRef: pdu.GocbRef,
	}

	s.mu.Lock()
	state, ok := s.streams[key]
	if !ok {
		state = &streamState{sub: sub}
		s.streams[key] = state
	}

	prevStNum := state.lastStNum
	prevSqNum := state.lastSqNum
	prevConfRev := state.lastConfRev
	wasSeen := state.seen
	wasLost := state.lost

	// Is this the first frame of the stream?
	isFirst := !wasSeen
	// Compare stNum/sqNum as serial numbers. State is updated only after
	// classification: stale, reordered, or replayed frames must not roll back
	// the baseline or extend the watchdog deadline indefinitely.
	stateDistance := pdu.StNum - prevStNum
	isStateChange := wasSeen && stateDistance != 0 && stateDistance < (1<<31)
	isRestartCandidate := wasSeen && stateDistance >= (1<<31)
	// Publisher configuration change.
	isConfRevChange := wasSeen && pdu.ConfRev != prevConfRev
	// Repeated frame (a duplicate caused by PRP/HSR or aggressive retransmission).
	isDuplicate := wasSeen && pdu.StNum == prevStNum && pdu.SqNum == prevSqNum
	// Accept a heartbeat only when sqNum moves forward with serial wraparound.
	sqDistance := pdu.SqNum - prevSqNum
	isHeartbeat := wasSeen && pdu.StNum == prevStNum && sqDistance != 0 && sqDistance < (1<<31)
	isReorderedHeartbeat := wasSeen && pdu.StNum == prevStNum && !isDuplicate && !isHeartbeat

	lastTimestamp := time.Time{}
	if state.lastPDU != nil {
		lastTimestamp = state.lastPDU.Timestamp
	}
	staleTimestamp := !pdu.Timestamp.IsZero() && !lastTimestamp.IsZero() && pdu.Timestamp.Before(lastTimestamp)
	restartTimestampOK := pdu.Timestamp.IsZero() || lastTimestamp.IsZero() || pdu.Timestamp.After(lastTimestamp)
	isRestart := isRestartCandidate && pdu.SqNum == 0 && restartTimestampOK

	if isDuplicate || isReorderedHeartbeat || staleTimestamp || (isRestartCandidate && !isRestart) {
		s.mu.Unlock()
		s.log.Debug("goose ignored stale, duplicate or reordered frame",
			"sub", sub.Name,
			"st_num", pdu.StNum,
			"sq_num", pdu.SqNum,
			"prev_st_num", prevStNum,
			"prev_sq_num", prevSqNum,
		)
		return
	}

	// sqNum gap or rollback within the same stNum.
	var sqGap int32
	if isHeartbeat {
		sqGap = int32(sqDistance - 1)
	}

	state.lastStNum = pdu.StNum
	state.lastSqNum = pdu.SqNum
	state.lastConfRev = pdu.ConfRev
	state.lastSeenAt = receivedAt
	state.lastObservedAt = frame.ObservedAt
	state.talMs = pdu.TimeAllowedToLiveMs
	state.lastPDU = pdu
	state.seen = true
	state.lost = false
	s.observeLocked(sub.Name, pdu, frame.ObservedAt, isRestart)
	s.mu.Unlock()

	// Stream recovery after a loss is a separate event.
	if wasLost {
		s.sink.OnGooseEvent(Event{
			Reason:       ReasonStreamRestored,
			Subscription: sub.Name,
			PDU:          pdu,
			ReceivedAt:   receivedAt,
		})
	}

	// Select the event reason. Order matters: restart > confRev > state_change.
	reason := EventReason("")
	switch {
	case isFirst:
		reason = ReasonFirstSeen
	case isRestart:
		reason = ReasonPublisherRestart
	case isStateChange:
		reason = ReasonStateChange
	case isConfRevChange:
		// conf_rev may arrive with a state_change, which was already selected above.
		reason = ReasonConfRevChanged
	case isHeartbeat:
		// Normal retransmission: emit no event, but keep tracking gaps and repeats in the log.
		if sqGap != 0 {
			s.log.Warn("goose sqNum gap",
				"sub", sub.Name, "st_num", pdu.StNum,
				"prev_sq_num", prevSqNum, "sq_num", pdu.SqNum, "gap", sqGap)
		}
		return
	default:
		return
	}

	s.sink.OnGooseEvent(Event{
		Reason:        reason,
		Subscription:  sub.Name,
		PDU:           pdu,
		ReceivedAt:    receivedAt,
		PreviousStNum: prevStNum,
		SqNumGap:      sqGap,
	})
}

// watchdog periodically checks streams for TimeAllowedToLive expiration
// and publishes stream_lost events.
func (s *Service) watchdog(ctx context.Context) {
	tick := time.NewTicker(watchdogTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.checkStale(now)
		}
	}
}

// checkStale scans streams and emits stream_lost for expired ones.
func (s *Service) checkStale(now time.Time) {
	type lostItem struct {
		sub *goose.Subscription
		pdu *goose.PDU
	}
	var lost []lostItem

	s.mu.Lock()
	for _, st := range s.streams {
		if !st.seen || st.lost {
			continue
		}
		deadline := st.lastSeenAt.Add(time.Duration(st.talMs) * time.Millisecond)
		if now.After(deadline) {
			st.lost = true
			lost = append(lost, lostItem{sub: st.sub, pdu: st.lastPDU})
		}
	}
	s.mu.Unlock()

	for _, it := range lost {
		s.sink.OnGooseEvent(Event{
			Reason:       ReasonStreamLost,
			Subscription: it.sub.Name,
			PDU:          it.pdu,
			ReceivedAt:   now,
		})
	}
}

// noopSink is the default sink that discards events.
type noopSink struct{}

func (noopSink) OnGooseEvent(Event) {}
