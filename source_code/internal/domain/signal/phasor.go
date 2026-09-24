// Package signal contains pure signal-processing and electrical calculations.
// It deliberately has no dependencies on UI, configuration or transport code.
package signal

import "math"

// Polar represents an RMS phasor in polar form.
type Polar struct {
	Magnitude float64
	AngleDeg  float64
}

// Phasor represents a complex RMS phasor.
type Phasor struct {
	Real float64
	Imag float64
}

// FromPolar converts a polar RMS value to a complex phasor.
func FromPolar(value Polar) Phasor {
	angle := value.AngleDeg * math.Pi / 180
	return Phasor{
		Real: value.Magnitude * math.Cos(angle),
		Imag: value.Magnitude * math.Sin(angle),
	}
}

// Polar converts a complex phasor to an RMS magnitude and an angle in [0, 360).
func (p Phasor) Polar() Polar {
	magnitude := math.Hypot(p.Real, p.Imag)
	if magnitude == 0 {
		return Polar{}
	}
	angle := math.Atan2(p.Imag, p.Real) * 180 / math.Pi
	if angle < 0 {
		angle += 360
	}
	return Polar{Magnitude: magnitude, AngleDeg: angle}
}

// Add sums complex phasors.
func Add(values ...Phasor) Phasor {
	var sum Phasor
	for _, value := range values {
		sum.Real += value.Real
		sum.Imag += value.Imag
	}
	return sum
}

// Rotate rotates a phasor counter-clockwise by angleDeg.
func Rotate(value Phasor, angleDeg float64) Phasor {
	angle := angleDeg * math.Pi / 180
	return Phasor{
		Real: value.Real*math.Cos(angle) - value.Imag*math.Sin(angle),
		Imag: value.Real*math.Sin(angle) + value.Imag*math.Cos(angle),
	}
}

// Neutral returns the neutral phasor -(a+b+c).
func Neutral(a, b, c Polar) Polar {
	sum := Add(FromPolar(a), FromPolar(b), FromPolar(c))
	return (Phasor{Real: -sum.Real, Imag: -sum.Imag}).Polar()
}

// SymmetricalComponentsToPhases converts positive-, negative- and zero-sequence
// components to phase A, B and C phasors.
func SymmetricalComponentsToPhases(positive, negative, zero Polar) [3]Polar {
	u1 := FromPolar(positive)
	u2 := FromPolar(negative)
	u0 := FromPolar(zero)
	return [3]Polar{
		Add(u0, u1, u2).Polar(),
		Add(u0, Rotate(u1, 240), Rotate(u2, 120)).Polar(),
		Add(u0, Rotate(u1, 120), Rotate(u2, 240)).Polar(),
	}
}
