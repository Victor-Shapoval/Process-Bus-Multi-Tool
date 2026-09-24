package svpub

import "pbmt/internal/domain/sv"

type Snapshot struct {
	Settings   [sv.NumChannels]ChannelSetting
	Simulation bool
	Synch      sv.SmpSynch
	Ready      bool
	Timing     TimingStats
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	ready := false
	select {
	case <-s.ready:
		ready = true
	default:
	}
	return Snapshot{Settings: s.settings, Simulation: s.sim, Synch: s.smpSynch, Ready: ready, Timing: s.timing}
}
