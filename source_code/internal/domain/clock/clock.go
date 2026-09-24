// Package clock defines the local clock interface and helper functions.
// Implementations (system clock and PHC) live in infrastructure.
package clock

import "time"

// Source provides time to application modules.
type Source interface {
	Now() time.Time
}

// SystemSource returns CLOCK_REALTIME without PTP correction.
type SystemSource struct{}

func (SystemSource) Now() time.Time { return time.Now().UTC() }

// SourceStatus describes the active application time source.
type SourceStatus struct {
	PTPEnabled   bool
	Synchronized bool
	Offset       time.Duration
	FrequencyPPB float64
	UpdatedAt    time.Time
}

// StatusSource is a source that can report its synchronization status.
type StatusSource interface {
	Source
	SourceStatus() SourceStatus
}

// StatusOf returns the source status. A plain Source is treated as the system
// clock.
func StatusOf(source Source) SourceStatus {
	if source == nil {
		return SourceStatus{}
	}
	if statusSource, ok := source.(StatusSource); ok {
		return statusSource.SourceStatus()
	}
	return SourceStatus{}
}

// Clock abstracts a local clock controlled by the PTP client.
type Clock interface {
	Source
	// FromSystem converts a transport RX/TX timestamp from CLOCK_REALTIME
	// into this clock's time scale.
	FromSystem(t time.Time) time.Time
	// Step shifts the clock by offset (positive means forward).
	Step(offset time.Duration) error
	// AdjustFrequency corrects the frequency in ppb (parts per billion).
	AdjustFrequency(ppb float64) error
	// MaxFreqAdj returns the maximum permitted frequency correction in ppb.
	MaxFreqAdj() float64
}

// SyncController lets the PTP client publish its lock state.
type SyncController interface {
	SetSynchronized(synchronized bool)
}
