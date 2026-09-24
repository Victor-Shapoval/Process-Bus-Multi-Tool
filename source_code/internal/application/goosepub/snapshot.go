package goosepub

import (
	"pbmt/internal/domain/goose"
	"time"
)

type Snapshot struct {
	Data             []goose.DataValue
	Test, Simulation bool
	ChangedAt        time.Time
}

// Snapshot returns configured output, not proof that a terminal received it.
func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{goose.CloneDataValues(s.logicalData), s.test, s.sim, s.stateChanged}
}
