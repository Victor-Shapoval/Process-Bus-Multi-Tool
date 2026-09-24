package svsub

import "time"

type Snapshot struct {
	ReceivedAt time.Time
	MeasuredAt time.Time
	Stale      bool
	RMSReady   bool
	Stats      *StreamStats
}

// Snapshots copies the existing periodic calculation; reading via MCP never
// advances the phasor estimator or depends on GUI event delivery.
func (s *Service) Snapshots(name string, now time.Time) []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Snapshot
	for _, st := range s.streams {
		if !st.seen || st.sub.Name != name {
			continue
		}
		v := Snapshot{ReceivedAt: st.lastSeenAt, MeasuredAt: st.measuredAt,
			Stale: st.lost || now.Sub(st.lastSeenAt) > streamLostTimeout, RMSReady: st.rmsReady}
		if st.latestStats != nil {
			copy := *st.latestStats
			v.Stats = &copy
		}
		out = append(out, v)
	}
	return out
}
