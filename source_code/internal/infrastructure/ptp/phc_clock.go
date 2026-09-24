package ptp

import (
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	minPHCRateSampleInterval = 10 * time.Millisecond
	initialPHCCalibration    = 100 * time.Millisecond
	maxPHCRateRatioError     = 0.01 // tolerate free-running PHCs up to ±10,000 ppm
	maxPHCRateResidual       = 20 * time.Microsecond
	maxHardwareTimestampAge  = 250 * time.Millisecond
)

var (
	errPHCClockDiscontinuity = errors.New("PTP hardware clock discontinuity")
	errPHCClockUncalibrated  = errors.New("PTP hardware clock rate is not calibrated")
)

// phcCrossTimestamp is one simultaneous sample of the NIC PHC, CLOCK_REALTIME
// and CLOCK_MONOTONIC_RAW returned by PTP_SYS_OFFSET_PRECISE.
type phcCrossTimestamp struct {
	Device   time.Time
	Realtime time.Time
	Monoraw  time.Time
}

// autonomousPHCClock starts with the host's UTC epoch, then advances only on
// CLOCK_MONOTONIC_RAW. Hardware event timestamps are mapped into that clock by
// measuring the PHC/monotonic frequency ratio. Later wall-clock changes cannot
// step an active Grandmaster.
type autonomousPHCClock struct {
	epochRealtime time.Time
	epochMonoraw  time.Time

	rateDevice  time.Time
	rateMonoraw time.Time
	rate        float64
	rateValid   bool
}

func newAutonomousPHCClock(initial phcCrossTimestamp) (*autonomousPHCClock, error) {
	if initial.Device.IsZero() || initial.Realtime.IsZero() || initial.Monoraw.IsZero() {
		return nil, errors.New("PTP cross-timestamp contains a zero clock value")
	}
	return &autonomousPHCClock{
		epochRealtime: initial.Realtime.UTC(),
		epochMonoraw:  initial.Monoraw,
		rateDevice:    initial.Device,
		rateMonoraw:   initial.Monoraw,
		rate:          1,
	}, nil
}

// Calibrate updates the PHC-to-MONOTONIC_RAW rate. Samples that are too close
// are retained for the next wider observation window.
func (c *autonomousPHCClock) Calibrate(sample phcCrossTimestamp) error {
	deviceDelta := sample.Device.Sub(c.rateDevice)
	monoDelta := sample.Monoraw.Sub(c.rateMonoraw)
	if deviceDelta <= 0 || monoDelta <= 0 {
		c.resetRateAnchor(sample)
		return fmt.Errorf("%w: non-increasing cross-timestamp", errPHCClockDiscontinuity)
	}
	if deviceDelta < minPHCRateSampleInterval || monoDelta < minPHCRateSampleInterval {
		return nil
	}

	candidate := float64(monoDelta) / float64(deviceDelta)
	if math.IsNaN(candidate) || math.IsInf(candidate, 0) || math.Abs(candidate-1) > maxPHCRateRatioError {
		c.resetRateAnchor(sample)
		return fmt.Errorf("%w: PHC rate ratio %.9f is outside the supported range", errPHCClockDiscontinuity, candidate)
	}
	if c.rateValid {
		predictedMono := c.rateMonoraw.Add(scaleClockDuration(deviceDelta, c.rate))
		if residual := absDuration(sample.Monoraw.Sub(predictedMono)); residual > maxPHCRateResidual {
			c.resetRateAnchor(sample)
			return fmt.Errorf("%w: PHC rate residual %s", errPHCClockDiscontinuity, residual)
		}
	}

	c.rate = candidate
	c.rateValid = true
	c.rateDevice = sample.Device
	c.rateMonoraw = sample.Monoraw
	return nil
}

func (c *autonomousPHCClock) resetRateAnchor(sample phcCrossTimestamp) {
	c.rateDevice = sample.Device
	c.rateMonoraw = sample.Monoraw
	c.rateValid = false
}

// Convert maps a raw PHC packet timestamp into the autonomous Grandmaster
// clock. sample must be a fresh cross-timestamp taken after the packet event.
func (c *autonomousPHCClock) Convert(raw time.Time, sample phcCrossTimestamp) (time.Time, error) {
	if raw.IsZero() || sample.Device.IsZero() || sample.Monoraw.IsZero() {
		return time.Time{}, errors.New("PTP timestamp conversion received a zero clock value")
	}
	age := sample.Device.Sub(raw)
	if age < -time.Millisecond || age > maxHardwareTimestampAge {
		return time.Time{}, fmt.Errorf("PTP hardware timestamp age %s is outside the supported range", age)
	}
	if err := c.Calibrate(sample); err != nil {
		return time.Time{}, err
	}
	if !c.rateValid {
		return time.Time{}, errPHCClockUncalibrated
	}

	serverAtSample := c.Now(sample.Monoraw)
	return serverAtSample.Add(scaleClockDuration(raw.Sub(sample.Device), c.rate)).UTC(), nil
}

// Now returns the autonomous clock at a supplied CLOCK_MONOTONIC_RAW value.
func (c *autonomousPHCClock) Now(monoraw time.Time) time.Time {
	return c.epochRealtime.Add(monoraw.Sub(c.epochMonoraw)).UTC()
}

func scaleClockDuration(value time.Duration, rate float64) time.Duration {
	return time.Duration(math.Round(float64(value) * rate))
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
