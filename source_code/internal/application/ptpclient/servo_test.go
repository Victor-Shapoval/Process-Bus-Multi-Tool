package ptpclient

import (
	"math"
	"testing"
)

func TestPIServoUnlockedThenLocked(t *testing.T) {
	s := NewPIServo(PIServoConfig{
		MaxFrequency:  500_000,
		StepThreshold: 1.0,
		SoftwareTS:    false, // hwts: kiScale=0.3, freq_est_interval ~53ms
	})
	s.SyncInterval(1.0)

	// The first sample leaves the servo UNLOCKED.
	_, state := s.Sample(1000, 1_000_000_000)
	if state != ServoUnlocked {
		t.Fatalf("sample 0: want UNLOCKED, got %s", state)
	}

	// After 2 s, the second sample must let the servo estimate drift and become LOCKED.
	_, state = s.Sample(900, 3_000_000_000)
	if state != ServoLocked {
		t.Fatalf("sample 1: want LOCKED, got %s", state)
	}

	// The third sample exercises the PI controller.
	ppb, state := s.Sample(500, 4_000_000_000)
	if state != ServoLocked {
		t.Fatalf("sample 2: want LOCKED, got %s", state)
	}
	if math.IsNaN(ppb) || math.IsInf(ppb, 0) {
		t.Fatalf("sample 2: bad ppb: %f", ppb)
	}
}

func TestPIServoJumpOnLargeOffset(t *testing.T) {
	s := NewPIServo(PIServoConfig{
		MaxFrequency:       500_000,
		StepThreshold:      0.5, // 500ms
		FirstStepThreshold: 0.0,
		SoftwareTS:         false, // hwts for fast freq estimation
	})
	s.SyncInterval(1.0)

	s.Sample(0, 1_000_000_000)
	// Second sample: offset 2s > step_threshold 500ms
	_, state := s.Sample(2_000_000_000, 3_000_000_000)
	if state != ServoJump {
		t.Fatalf("want JUMP for large offset, got %s", state)
	}
}

func TestPIServoSoftwareTSNeedsMoreTime(t *testing.T) {
	// With software timestamps, kiScale=0.001 and freq_est_interval is about 16 s.
	s := NewPIServo(PIServoConfig{
		MaxFrequency: 500_000,
		SoftwareTS:   true,
	})
	s.SyncInterval(1.0)

	s.Sample(1000, 1_000_000_000)
	// 2 s is insufficient, so the servo remains UNLOCKED.
	_, state := s.Sample(900, 3_000_000_000)
	if state != ServoUnlocked {
		t.Fatalf("want UNLOCKED (too short for swts), got %s", state)
	}

	// 20 s is sufficient.
	_, state = s.Sample(800, 21_000_000_000)
	if state != ServoLocked {
		t.Fatalf("want LOCKED after 20s, got %s", state)
	}
}

func TestPIServoReset(t *testing.T) {
	s := NewPIServo(PIServoConfig{
		MaxFrequency: 500_000,
		SoftwareTS:   false,
	})
	s.SyncInterval(1.0)

	s.Sample(100, 1_000_000_000)
	s.Sample(90, 3_000_000_000)
	s.Reset()

	// After reset, should be back to UNLOCKED
	_, state := s.Sample(50, 5_000_000_000)
	if state != ServoUnlocked {
		t.Fatalf("after reset: want UNLOCKED, got %s", state)
	}
}

func TestPIServoFrequencyClamping(t *testing.T) {
	s := NewPIServo(PIServoConfig{
		MaxFrequency: 100,
		SoftwareTS:   false,
	})
	s.SyncInterval(1.0)

	s.Sample(0, 1_000_000_000)
	ppb, _ := s.Sample(1_000_000, 3_000_000_000)

	if ppb < -100 || ppb > 100 {
		t.Fatalf("ppb %f exceeds max_frequency 100", ppb)
	}
}
