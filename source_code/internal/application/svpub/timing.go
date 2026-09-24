package svpub

import (
	"context"
	"time"
)

// Monitoring is deliberately off the send path: file I/O must not delay SV.
func (s *Service) monitorTiming(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.timing.FramesPerSecond = 0
		s.mu.Unlock()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	previous := s.SchedulerStats()
	previousTime := time.Now()
	lastWarning := previousTime
	var skipped uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			current := s.sampleTiming(previous, now.Sub(previousTime))
			skipped += current.SkippedSamples - previous.SkippedSamples
			if skipped > 0 && now.Sub(lastWarning) >= 5*time.Second {
				s.log.Warn("sv_pub: sample slots missed",
					"name", s.cfg.Name, "skipped_samples", skipped,
					"frames_per_second", current.FramesPerSecond,
					"target_frames_per_second", s.cfg.samplesPerSecond(),
					"max_lateness", current.MaxLateness)
				skipped = 0
				lastWarning = now
			}
			previous, previousTime = current, now
		}
	}
}

func (s *Service) sampleTiming(previous TimingStats, elapsed time.Duration) TimingStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if elapsed > 0 {
		s.timing.FramesPerSecond = float64(s.timing.FramesSent-previous.FramesSent) / elapsed.Seconds()
	}
	return s.timing
}
