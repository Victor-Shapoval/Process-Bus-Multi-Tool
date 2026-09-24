package svpub

import (
	"testing"
	"time"
)

func TestTimingRateAndSnapshot(t *testing.T) {
	s := newTestService(&mockSink{})
	s.timing = TimingStats{FramesSent: 8100, SkippedSamples: 100, MaxLateness: time.Millisecond}
	stats := s.sampleTiming(TimingStats{FramesSent: 100}, 2*time.Second)
	if stats.FramesPerSecond != 4000 || stats.SkippedSamples != 100 || stats.MaxLateness != time.Millisecond {
		t.Fatalf("incorrect timing: %+v", stats)
	}
	if s.Snapshot().Timing != stats || s.SchedulerStats() != stats {
		t.Fatal("timing snapshots disagree")
	}
	// A stalled publisher must not keep reporting its last healthy frame rate.
	if got := s.sampleTiming(stats, time.Second); got.FramesPerSecond != 0 {
		t.Fatalf("stalled publisher reports traffic: %+v", got)
	}
}
