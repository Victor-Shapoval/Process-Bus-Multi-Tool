package svpub

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"pbmt/internal/domain/sv"
)

// This opt-in host diagnostic uses the real scheduler and encoder, but never
// opens a network interface. It is not a deterministic CI performance test.
func TestPublisherHostTiming(t *testing.T) {
	if os.Getenv("PBMT_TEST_HOST_TIMING") != "1" {
		t.Skip("set PBMT_TEST_HOST_TIMING=1 to measure host scheduling (no network traffic)")
	}
	for _, hz := range []uint16{50, 60} {
		for _, streams := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dHz_%dstreams", hz, streams), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				services := make([]*Service, streams)
				sinks := make([]*timingSink, streams)
				done := make(chan error, streams)
				for i := range services {
					svc := newTestService(&mockSink{})
					svc.cfg.SampleTimingFrequency = hz
					sinks[i] = &timingSink{}
					svc.sink = sinks[i]
					var settings [sv.NumChannels]ChannelSetting
					for ch := range settings {
						settings[ch] = ChannelSetting{RMS: 400, Frequency: float64(hz)}
					}
					if err := svc.SetWaveform(settings, false, sv.SmpSynchNone); err != nil {
						t.Fatal(err)
					}
					services[i] = svc
					go func() { done <- svc.Run(ctx) }()
				}
				for range services {
					if err := <-done; err != nil {
						t.Error(err)
					}
				}
				for i, svc := range services {
					stats := svc.SchedulerStats()
					if stats.FramesSent < 2 {
						t.Errorf("stream %d never started: %+v", i, stats)
						continue
					}
					fps := float64(stats.FramesSent-1) / sinks[i].last.Sub(sinks[i].first).Seconds()
					t.Logf("stream=%d fps=%.1f target=%d sent=%d skipped=%d max_lateness=%s",
						i, fps, int(hz)*80, stats.FramesSent, stats.SkippedSamples, stats.MaxLateness)
				}
			})
		}
	}
}

type timingSink struct{ first, last time.Time }

func (s *timingSink) WriteFrame([]byte) error {
	now := time.Now()
	if s.first.IsZero() {
		s.first = now
	}
	s.last = now
	return nil
}
