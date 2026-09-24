package goosesub

import (
	"time"

	"pbmt/internal/domain/goose"
)

// Snapshot is a detached copy of accepted receiver state, including heartbeats
// that deliberately do not produce GUI events.
type Snapshot struct {
	PDU        *goose.PDU
	ReceivedAt time.Time
	ObservedAt time.Time
	Stale      bool
}

func (s *Service) Snapshots(name string, now time.Time) []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snapshot
	for _, st := range s.streams {
		if !st.seen || st.sub.Name != name || st.lastPDU == nil {
			continue
		}
		out = append(out, Snapshot{PDU: goose.ClonePDU(st.lastPDU), ReceivedAt: st.lastSeenAt, ObservedAt: st.lastObservedAt,
			Stale: st.lost || now.Sub(st.lastSeenAt) > time.Duration(st.talMs)*time.Millisecond})
	}
	return out
}
