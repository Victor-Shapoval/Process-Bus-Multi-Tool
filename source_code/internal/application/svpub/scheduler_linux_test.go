//go:build linux

package svpub

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxSchedulerDeadlineAndCancellation(t *testing.T) {
	s := newPublisherScheduler()
	defer s.Stop()
	deadline := time.Now().Add(3 * time.Millisecond)
	if !s.WaitUntil(context.Background(), deadline) || time.Now().Before(deadline) {
		t.Fatal("wait failed or returned before deadline", s.Err())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.WaitUntil(ctx, time.Now().Add(-time.Second)) {
		t.Fatal("cancelled wait succeeded")
	}
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(time.Millisecond, cancel)
	started := time.Now()
	if s.WaitUntil(ctx, started.Add(time.Hour)) {
		t.Fatal("cancelled long wait succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation did not interrupt long wait")
	}
}

func TestLinuxSchedulerRetriesAbsoluteDeadlineAndReportsErrors(t *testing.T) {
	s := newPublisherScheduler()
	defer s.Stop()
	var target unix.Timespec
	calls := 0
	s.clockSleep = func(clock int32, flags int, request, remaining *unix.Timespec) error {
		if clock != unix.CLOCK_MONOTONIC || flags != unix.TIMER_ABSTIME || remaining != nil {
			t.Fatal("not an absolute monotonic sleep")
		}
		calls++
		if calls == 1 {
			target = *request
			return unix.EINTR
		}
		if target != *request {
			t.Fatal("interruption changed absolute target")
		}
		return unix.EINVAL
	}
	if s.WaitUntil(context.Background(), time.Now().Add(time.Hour)) || !errors.Is(s.Err(), unix.EINVAL) || calls != 2 {
		t.Fatalf("sleep failure was lost: calls=%d err=%v", calls, s.Err())
	}
}

func TestLinuxSchedulerClockFailure(t *testing.T) {
	s := newPublisherScheduler()
	defer s.Stop()
	s.clockGettime = func(int32, *unix.Timespec) error { return unix.EINVAL }
	if s.WaitUntil(context.Background(), time.Now().Add(time.Second)) || !errors.Is(s.Err(), unix.EINVAL) {
		t.Fatal("clock failure was lost", s.Err())
	}
}
