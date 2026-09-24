package signal

import (
	"math"
	"testing"
)

func TestBalancedPositiveSequence(t *testing.T) {
	phases := SymmetricalComponentsToPhases(
		Polar{Magnitude: 100, AngleDeg: 0},
		Polar{},
		Polar{},
	)
	wantAngles := [3]float64{0, 240, 120}
	for i, phase := range phases {
		if math.Abs(phase.Magnitude-100) > 1e-9 {
			t.Fatalf("phase %d magnitude: got %v, want 100", i, phase.Magnitude)
		}
		if angularDistance(phase.AngleDeg, wantAngles[i]) > 1e-9 {
			t.Fatalf("phase %d angle: got %v, want %v", i, phase.AngleDeg, wantAngles[i])
		}
	}
}

func TestNeutral(t *testing.T) {
	neutral := Neutral(
		Polar{Magnitude: 10, AngleDeg: 0},
		Polar{Magnitude: 10, AngleDeg: 180},
		Polar{Magnitude: 5, AngleDeg: 90},
	)
	if math.Abs(neutral.Magnitude-5) > 1e-9 {
		t.Fatalf("magnitude: got %v, want 5", neutral.Magnitude)
	}
	if angularDistance(neutral.AngleDeg, 270) > 1e-9 {
		t.Fatalf("angle: got %v, want 270", neutral.AngleDeg)
	}
}

func angularDistance(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	return math.Min(d, 360-d)
}
