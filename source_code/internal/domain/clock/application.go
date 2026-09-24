package clock

import (
	"errors"
	"sync"
	"time"
)

// ApplicationClock is the process-internal clock. It does not modify system
// time: PTP adjusts only the internal time scale.
// When PTP is disabled, Now and FromSystem return system time.
type ApplicationClock struct {
	mu sync.RWMutex

	ptpEnabled   bool
	synchronized bool
	baseSystem   time.Time
	baseApp      time.Time
	frequencyPPB float64
	maxFreqAdj   float64
	updatedAt    time.Time
	localMode    bool
	localSource  func() (time.Time, error)
	localError   error
}

var _ Clock = (*ApplicationClock)(nil)
var _ StatusSource = (*ApplicationClock)(nil)
var _ SyncController = (*ApplicationClock)(nil)

// NewApplicationClock creates a clock with a frequency-adjustment limit.
func NewApplicationClock(maxPPB float64) *ApplicationClock {
	if maxPPB <= 0 {
		maxPPB = 500_000
	}
	now := time.Now()
	return &ApplicationClock{
		baseSystem: now,
		baseApp:    now.UTC(),
		maxFreqAdj: maxPPB,
		updatedAt:  now,
	}
}

// EnablePTP enables the internal time scale starting at the current system
// time. The servo then calculates the offset and frequency.
func (c *ApplicationClock) EnablePTP() {
	now := time.Now()
	c.mu.Lock()
	c.ptpEnabled = true
	c.localMode, c.localSource, c.localError = false, nil, nil
	c.synchronized = false
	c.baseSystem = now
	c.baseApp = now.UTC()
	c.frequencyPPB = 0
	c.updatedAt = now
	c.mu.Unlock()
}

// DisablePTP immediately returns application modules to system time.
func (c *ApplicationClock) DisablePTP() {
	c.mu.Lock()
	c.ptpEnabled = false
	c.localMode, c.localSource, c.localError = false, nil, nil
	c.synchronized = false
	c.frequencyPPB = 0
	c.updatedAt = time.Now()
	c.mu.Unlock()
}

func (c *ApplicationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.localMode {
		return c.localNowLocked()
	}
	if !c.ptpEnabled {
		return time.Now().UTC()
	}
	return c.fromSystemLocked(time.Now())
}

func (c *ApplicationClock) FromSystem(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.localMode {
		now := time.Now()
		return c.localNowLocked().Add(t.Sub(now))
	}
	if !c.ptpEnabled {
		return t.UTC()
	}
	return c.fromSystemLocked(t)
}

func (c *ApplicationClock) fromSystemLocked(t time.Time) time.Time {
	elapsed := t.Sub(c.baseSystem)
	scaled := time.Duration(float64(elapsed) * (1 + c.frequencyPPB/1_000_000_000))
	return c.baseApp.Add(scaled).UTC()
}

func (c *ApplicationClock) Step(offset time.Duration) error {
	now := time.Now()
	c.mu.Lock()
	current := c.fromSystemLocked(now)
	c.baseSystem = now
	c.baseApp = current.Add(offset).UTC()
	c.updatedAt = now
	c.mu.Unlock()
	return nil
}

func (c *ApplicationClock) AdjustFrequency(ppb float64) error {
	if ppb > c.maxFreqAdj {
		ppb = c.maxFreqAdj
	} else if ppb < -c.maxFreqAdj {
		ppb = -c.maxFreqAdj
	}
	now := time.Now()
	c.mu.Lock()
	current := c.fromSystemLocked(now)
	c.baseSystem = now
	c.baseApp = current
	c.frequencyPPB = ppb
	c.updatedAt = now
	c.mu.Unlock()
	return nil
}

func (c *ApplicationClock) MaxFreqAdj() float64 { return c.maxFreqAdj }

func (c *ApplicationClock) SetSynchronized(synchronized bool) {
	c.mu.Lock()
	c.synchronized = c.ptpEnabled && synchronized
	c.updatedAt = time.Now()
	c.mu.Unlock()
}

func (c *ApplicationClock) SourceStatus() SourceStatus {
	systemNow := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ptpEnabled {
		return SourceStatus{UpdatedAt: c.updatedAt}
	}
	appNow := c.fromSystemLocked(systemNow)
	if c.localMode {
		appNow = c.localNowLocked()
	}
	return SourceStatus{
		PTPEnabled:   true,
		Synchronized: c.synchronized,
		Offset:       appNow.Sub(systemNow),
		FrequencyPPB: c.frequencyPPB,
		UpdatedAt:    c.updatedAt,
	}
}

// BindLocalSource follows an existing server clock without a network servo.
// The callback must not call back into ApplicationClock or its owner. A source
// failure is latched: time then free-runs from the last sample, unsynchronized,
// until an explicit restart. There is no fallback to system or external PTP.
func (c *ApplicationClock) BindLocalSource(source func() (time.Time, error)) error {
	if source == nil {
		return errors.New("local PTP source is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, err := source()
	if err != nil {
		return err
	}
	if value.IsZero() {
		return errors.New("local PTP source returned an empty time")
	}
	c.ptpEnabled, c.synchronized, c.localMode = true, true, true
	c.localSource, c.localError = source, nil
	c.baseSystem, c.baseApp = time.Now(), value.UTC()
	c.frequencyPPB = 0
	c.updatedAt = c.baseSystem
	return nil
}

func (c *ApplicationClock) localNowLocked() time.Time {
	if c.localSource != nil {
		value, err := c.localSource()
		if err == nil && value.IsZero() {
			err = errors.New("local PTP source returned an empty time")
		}
		if err == nil {
			c.baseSystem, c.baseApp = time.Now(), value.UTC()
			c.updatedAt = c.baseSystem
			return c.baseApp
		}
		c.localSource, c.localError = nil, err
		c.synchronized = false
		c.updatedAt = time.Now()
	}
	// baseSystem carries Go's monotonic timestamp, so wall-clock changes do
	// not switch the held-over local scale back to system time.
	return c.baseApp.Add(time.Since(c.baseSystem))
}

// LocalSourceError refreshes the binding and reports a latched source loss.
func (c *ApplicationClock) LocalSourceError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.localMode {
		c.localNowLocked()
	}
	return c.localError
}
