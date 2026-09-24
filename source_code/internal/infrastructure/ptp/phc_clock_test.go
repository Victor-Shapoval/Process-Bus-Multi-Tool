package ptp

import (
	"errors"
	"testing"
	"time"
)

func TestAutonomousPHCClockCorrectsMeasuredRateError(t *testing.T) {
	epoch := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	deviceEpoch := time.Unix(10, 0).UTC()
	monoEpoch := time.Unix(100, 0).UTC()
	clock, err := newAutonomousPHCClock(phcCrossTimestamp{
		Device: deviceEpoch, Realtime: epoch, Monoraw: monoEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}

	// On the target host, realtime-minus-PHC grew by 1,594.743 ppm, so the
	// free-running PHC was slower than the host clock by that measured rate.
	const phcRate = 1 - 1594.743/1_000_000
	monoElapsed := time.Second
	deviceElapsed := time.Duration(float64(monoElapsed) * phcRate)
	sample := phcCrossTimestamp{
		Device:   deviceEpoch.Add(deviceElapsed),
		Realtime: epoch.Add(monoElapsed),
		Monoraw:  monoEpoch.Add(monoElapsed),
	}
	eventAge := time.Millisecond
	raw := sample.Device.Add(-time.Duration(float64(eventAge) * phcRate))

	got, err := clock.Convert(raw, sample)
	if err != nil {
		t.Fatal(err)
	}
	want := epoch.Add(monoElapsed - eventAge)
	if delta := absDuration(got.Sub(want)); delta > time.Nanosecond {
		t.Fatalf("affine conversion error: got %v, want %v (delta %s)", got, want, delta)
	}

	// A scalar offset sampled one millisecond after the event would be wrong by
	// roughly 1.595 microseconds on this PHC.
	scalar := raw.Add(sample.Realtime.Sub(sample.Device))
	if error := absDuration(scalar.Sub(want)); error < time.Microsecond {
		t.Fatalf("test setup did not reproduce stale scalar error: %s", error)
	}
}

func TestAutonomousPHCClockIgnoresRealtimeStepAfterStart(t *testing.T) {
	epoch := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	initial := phcCrossTimestamp{
		Device: time.Unix(10, 0), Realtime: epoch, Monoraw: time.Unix(100, 0),
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		t.Fatal(err)
	}
	sample := phcCrossTimestamp{
		Device:   initial.Device.Add(time.Second),
		Realtime: initial.Realtime.Add(6 * time.Second), // host clock stepped +5 s
		Monoraw:  initial.Monoraw.Add(time.Second),
	}
	got, err := clock.Convert(sample.Device, sample)
	if err != nil {
		t.Fatal(err)
	}
	if want := epoch.Add(time.Second); !got.Equal(want) {
		t.Fatalf("realtime step leaked into autonomous clock: want %v, got %v", want, got)
	}
}

func TestAutonomousPHCClockRejectsStaleTimestamp(t *testing.T) {
	initial := phcCrossTimestamp{
		Device: time.Unix(10, 0), Realtime: time.Unix(1_000, 0), Monoraw: time.Unix(100, 0),
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		t.Fatal(err)
	}
	sample := phcCrossTimestamp{
		Device: initial.Device.Add(time.Second), Realtime: initial.Realtime.Add(time.Second), Monoraw: initial.Monoraw.Add(time.Second),
	}
	_, err = clock.Convert(sample.Device.Add(-time.Second), sample)
	if err == nil {
		t.Fatal("expected stale hardware timestamp error")
	}
}

func TestAutonomousPHCClockRequiresAUsableRateWindow(t *testing.T) {
	initial := phcCrossTimestamp{
		Device: time.Unix(10, 0), Realtime: time.Unix(1_000, 0), Monoraw: time.Unix(100, 0),
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		t.Fatal(err)
	}
	tooClose := phcCrossTimestamp{
		Device: initial.Device.Add(time.Millisecond), Monoraw: initial.Monoraw.Add(time.Millisecond),
	}
	if _, err := clock.Convert(tooClose.Device, tooClose); !errors.Is(err, errPHCClockUncalibrated) {
		t.Fatalf("expected uncalibrated rate error, got %v", err)
	}
}

func TestAutonomousPHCClockRejectsRateDiscontinuityAndResetsAnchor(t *testing.T) {
	initial := phcCrossTimestamp{
		Device: time.Unix(10, 0), Realtime: time.Unix(1_000, 0), Monoraw: time.Unix(100, 0),
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		t.Fatal(err)
	}
	stable := phcCrossTimestamp{
		Device: initial.Device.Add(time.Second), Realtime: initial.Realtime.Add(time.Second), Monoraw: initial.Monoraw.Add(time.Second),
	}
	if _, err := clock.Convert(stable.Device, stable); err != nil {
		t.Fatal(err)
	}
	jumped := phcCrossTimestamp{
		Device: stable.Device.Add(2 * time.Second), Realtime: stable.Realtime.Add(time.Second), Monoraw: stable.Monoraw.Add(time.Second),
	}
	if _, err := clock.Convert(jumped.Device, jumped); !errors.Is(err, errPHCClockDiscontinuity) {
		t.Fatalf("expected PHC discontinuity, got %v", err)
	}
	recovered := phcCrossTimestamp{
		Device: jumped.Device.Add(time.Second), Realtime: jumped.Realtime.Add(time.Second), Monoraw: jumped.Monoraw.Add(time.Second),
	}
	if _, err := clock.Convert(recovered.Device, recovered); err != nil {
		t.Fatalf("converter did not recover after resetting its anchor: %v", err)
	}
}
