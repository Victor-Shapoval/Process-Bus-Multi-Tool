package ptpclient

import "math"

// ServoState is the clock servo state.
type ServoState int

const (
	ServoUnlocked     ServoState = iota // not ready for tracking yet
	ServoJump                           // a step is recommended
	ServoLocked                         // tracking the master
	ServoLockedStable                   // stably locked
)

func (s ServoState) String() string {
	switch s {
	case ServoUnlocked:
		return "UNLOCKED"
	case ServoJump:
		return "JUMP"
	case ServoLocked:
		return "LOCKED"
	case ServoLockedStable:
		return "LOCKED_STABLE"
	default:
		return "UNKNOWN"
	}
}

// PIServo is a proportional-integral clock servo.
type PIServo struct {
	offset   [2]int64
	local    [2]uint64
	drift    float64
	kp       float64
	ki       float64
	lastFreq float64
	count    int

	maxFrequency       float64
	stepThreshold      float64 // ns; 0 disables it
	firstStepThreshold float64 // ns; 0 disables it
	firstUpdate        bool

	kpScale    float64
	kpExponent float64
	kpNormMax  float64
	kiScale    float64
	kiExponent float64
	kiNormMax  float64

	// Stability detection.
	offsetThreshold  int64
	numOffsetValues  int
	currOffsetValues int
}

// PIServoConfig configures the PI servo.
type PIServoConfig struct {
	// Initial drift (frequency offset in ppb), usually 0.
	InitialDrift float64
	// Maximum frequency correction in ppb.
	MaxFrequency float64
	// Step threshold in seconds. 0 allows slew only.
	StepThreshold float64
	// First-step threshold in seconds. 0 uses StepThreshold.
	FirstStepThreshold float64
	// true selects software timestamping with more aggressive filtering.
	SoftwareTS bool
}

const (
	hwtsKPScale = 0.7
	hwtsKIScale = 0.3
	swtsKPScale = 0.1
	swtsKIScale = 0.001

	maxKPNormMax = 1.0
	maxKINormMax = 2.0

	freqEstMargin = 0.001
	nsPerSec      = 1e9
)

// NewPIServo creates a PI servo.
func NewPIServo(cfg PIServoConfig) *PIServo {
	s := &PIServo{
		drift:           cfg.InitialDrift,
		lastFreq:        cfg.InitialDrift,
		maxFrequency:    cfg.MaxFrequency,
		firstUpdate:     true,
		offsetThreshold: 0,
		numOffsetValues: 0,
	}

	if cfg.StepThreshold > 0 {
		s.stepThreshold = cfg.StepThreshold * nsPerSec
	}
	if cfg.FirstStepThreshold > 0 {
		s.firstStepThreshold = cfg.FirstStepThreshold * nsPerSec
	}

	if cfg.SoftwareTS {
		s.kpScale = swtsKPScale
		s.kiScale = swtsKIScale
	} else {
		s.kpScale = hwtsKPScale
		s.kiScale = hwtsKIScale
	}
	s.kpExponent = -0.3
	s.kpNormMax = maxKPNormMax
	s.kiExponent = 0.4
	s.kiNormMax = maxKINormMax

	if cfg.MaxFrequency <= 0 {
		s.maxFrequency = 500_000 // ±500 ppm
	}

	return s
}

// SyncInterval informs the servo of the Sync interval in seconds.
func (s *PIServo) SyncInterval(interval float64) {
	s.kp = s.kpScale * math.Pow(interval, s.kpExponent)
	if s.kp > s.kpNormMax/interval {
		s.kp = s.kpNormMax / interval
	}
	s.ki = s.kiScale * math.Pow(interval, s.kiExponent)
	if s.ki > s.kiNormMax/interval {
		s.ki = s.kiNormMax / interval
	}
}

// Sample feeds a measurement to the servo and returns the frequency correction in ppb.
func (s *PIServo) Sample(offset int64, localTS uint64) (ppb float64, state ServoState) {
	ppb = s.lastFreq

	switch s.count {
	case 0:
		s.offset[0] = offset
		s.local[0] = localTS
		state = ServoUnlocked
		s.count = 1

	case 1:
		s.offset[1] = offset
		s.local[1] = localTS

		if s.local[0] >= s.local[1] {
			state = ServoUnlocked
			s.count = 0
			break
		}

		localDiff := float64(s.local[1]-s.local[0]) / nsPerSec
		localDiff += localDiff * freqEstMargin
		freqEstInterval := 0.016 / s.ki
		if freqEstInterval > 1000.0 {
			freqEstInterval = 1000.0
		}
		if localDiff < freqEstInterval {
			state = ServoUnlocked
			break
		}

		// Estimate the initial drift.
		s.drift += (nsPerSec - s.drift) * float64(s.offset[1]-s.offset[0]) /
			float64(s.local[1]-s.local[0])

		s.drift = clampFloat(s.drift, -s.maxFrequency, s.maxFrequency)

		absOff := abs64(offset)
		if (s.firstUpdate && s.firstStepThreshold > 0 && float64(absOff) > s.firstStepThreshold) ||
			(s.stepThreshold > 0 && float64(absOff) > s.stepThreshold) {
			state = ServoJump
		} else {
			state = ServoLocked
		}
		ppb = s.drift
		s.count = 2

	case 2:
		absOff := abs64(offset)
		if s.stepThreshold > 0 && float64(absOff) > s.stepThreshold {
			state = ServoUnlocked
			s.count = 0
			break
		}

		kiTerm := s.ki * float64(offset)
		ppb = s.kp*float64(offset) + s.drift + kiTerm
		if ppb < -s.maxFrequency {
			ppb = -s.maxFrequency
		} else if ppb > s.maxFrequency {
			ppb = s.maxFrequency
		} else {
			s.drift += kiTerm
		}
		state = ServoLocked
	}

	s.lastFreq = ppb

	// Check stability.
	if state == ServoLocked && s.offsetThreshold > 0 {
		if abs64(offset) < s.offsetThreshold {
			s.currOffsetValues--
			if s.currOffsetValues <= 0 {
				state = ServoLockedStable
			}
		} else {
			s.currOffsetValues = s.numOffsetValues
		}
	}
	if state == ServoJump || state == ServoUnlocked {
		s.currOffsetValues = s.numOffsetValues
	}
	if state == ServoJump || state == ServoLocked {
		s.firstUpdate = false
	}

	return ppb, state
}

// Reset resets the servo after a master change or similar event.
func (s *PIServo) Reset() {
	s.count = 0
	s.firstUpdate = true
	s.currOffsetValues = s.numOffsetValues
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
