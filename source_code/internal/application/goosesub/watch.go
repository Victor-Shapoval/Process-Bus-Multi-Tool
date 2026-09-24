package goosesub

import (
	"sync"
	"time"

	"pbmt/internal/domain/goose"
)

// Observation is detached from receiver state. At uses the application
// monotonic clock sampled immediately after the capture read, before queuing.
type Observation struct {
	PDU     *goose.PDU
	At      time.Time
	Restart bool
}

// Watch is independent of the lossy GUI event bus. Overflow is explicit;
// consumers must fail the measurement, never silently report a later edge.
type Watch struct {
	Initial []Snapshot
	Frames  <-chan Observation
	Failed  <-chan struct{}
	Closed  <-chan struct{}
	Cancel  func()
}

type watchChannels struct {
	name   string
	frames chan Observation
	failed chan struct{}
	closed chan struct{}
}

// Observe atomically subscribes and takes a baseline. The caller must Cancel.
func (s *Service) Observe(name string) *Watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := watchChannels{name, make(chan Observation, 256), make(chan struct{}), make(chan struct{})}
	w := &Watch{Frames: c.frames, Failed: c.failed, Closed: c.closed}
	now := time.Now()
	for _, st := range s.streams {
		if st.seen && st.sub.Name == name {
			w.Initial = append(w.Initial, Snapshot{PDU: goose.ClonePDU(st.lastPDU), ReceivedAt: st.lastSeenAt, ObservedAt: st.lastObservedAt,
				Stale: st.lost || now.Sub(st.lastSeenAt) > time.Duration(st.talMs)*time.Millisecond})
		}
	}
	if s.watches == nil {
		s.watches = make(map[*Watch]watchChannels)
	}
	s.watches[w] = c
	var once sync.Once
	w.Cancel = func() { once.Do(func() { s.mu.Lock(); defer s.mu.Unlock(); delete(s.watches, w) }) }
	return w
}

func (s *Service) observeLocked(name string, pdu *goose.PDU, at time.Time, restart bool) {
	for _, c := range s.watches {
		if c.name != name {
			continue
		}
		select {
		case <-c.failed:
			continue
		default:
		}
		select {
		case c.frames <- Observation{PDU: goose.ClonePDU(pdu), At: at, Restart: restart}:
		default:
			close(c.failed)
		}
	}
}

func (s *Service) closeWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w, c := range s.watches {
		close(c.closed)
		delete(s.watches, w)
	}
}
