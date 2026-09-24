//go:build linux

package svpub

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// Leave enough time for the final spin to absorb normal Linux wakeup jitter.
	// This trades CPU time for timing precision without changing RT priorities.
	linuxSpinWindow = 150 * time.Microsecond
	linuxSleepChunk = 10 * time.Millisecond
)

type linuxScheduler struct {
	err error
	// Hooks keep interruption and failure handling deterministic in tests.
	clockGettime func(int32, *unix.Timespec) error
	clockSleep   func(int32, int, *unix.Timespec, *unix.Timespec) error
}

// Construction, waiting and Stop must all run on the publisher goroutine.
func newPublisherScheduler() *linuxScheduler {
	runtime.LockOSThread()
	return &linuxScheduler{clockGettime: unix.ClockGettime, clockSleep: unix.ClockNanosleep}
}

func (*linuxScheduler) Now() time.Time { return time.Now() }
func (*linuxScheduler) Stop()          { runtime.UnlockOSThread() }
func (s *linuxScheduler) Err() error   { return s.err }

func (s *linuxScheduler) WaitUntil(ctx context.Context, deadline time.Time) bool {
	spinWindow := linuxSpinWindow
	if time.Until(deadline) > linuxSleepChunk {
		// Whole-second startup needs a wider margin after a long sleep. This
		// extra spin occurs only at startup/rebasing, not on every sample.
		spinWindow = time.Millisecond
	}
	for ctx.Err() == nil && s.err == nil {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		if remaining <= spinWindow {
			// Do not yield here: a Go timer/yield can overshoot a 250 us slot.
			continue
		}
		var now unix.Timespec
		if err := s.clockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
			s.err = fmt.Errorf("sv_pub: read monotonic clock: %w", err)
			return false
		}
		// Go's monotonic epoch is private. Convert the remaining duration using
		// a fresh kernel reading; never pass Unix wall time to CLOCK_MONOTONIC.
		delay := time.Until(deadline) - spinWindow
		if delay <= 0 {
			continue
		}
		if delay > linuxSleepChunk {
			delay = linuxSleepChunk // Bound cancellation latency during startup.
		}
		target := unix.NsecToTimespec(now.Nano() + int64(delay))
		for ctx.Err() == nil {
			err := s.clockSleep(unix.CLOCK_MONOTONIC, unix.TIMER_ABSTIME, &target, nil)
			if err == unix.EINTR {
				continue // Retry the same absolute target, without accumulating drift.
			}
			if err != nil {
				s.err = fmt.Errorf("sv_pub: absolute monotonic sleep: %w", err)
				return false
			}
			break
		}
	}
	return false
}
