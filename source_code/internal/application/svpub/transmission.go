package svpub

import (
	"sync"
	"time"

	"pbmt/internal/domain/sv"
)

// SendReceipt marks the application send boundary, not NIC transmission.
// At is sampled before WriteFrame and is usable only when Err == nil.
// AppTime is the project clock for reporting; intervals use At's monotonic part.
type SendReceipt struct {
	At      time.Time
	AppTime time.Time
	Err     error
}

type transmissionTracker struct {
	out  chan SendReceipt
	once sync.Once
}

func (t *transmissionTracker) complete(r SendReceipt) { t.once.Do(func() { t.out <- r }) }

// SetWaveformTracked associates a receipt with the exact settings snapshot
// used by transmit. A frame already in flight cannot acknowledge this change.
func (s *Service) SetWaveformTracked(settings [sv.NumChannels]ChannelSetting, simulation bool, synch sv.SmpSynch) (<-chan SendReceipt, error) {
	ch := make(chan SendReceipt, 1)
	t := &transmissionTracker{out: ch}
	if err := s.setWaveform(settings, simulation, synch, t); err != nil {
		return nil, err
	}
	return ch, nil
}
