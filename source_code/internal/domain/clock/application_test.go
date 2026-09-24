package clock

import (
	"errors"
	"testing"
	"time"
)

func TestLocalSourceFollowsServerAndLatchesLossWithoutFallback(t *testing.T) {
	c := NewApplicationClock(500_000)
	value := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	available := true
	reads := 0
	source := func() (time.Time, error) {
		reads++
		if !available {
			return time.Time{}, errors.New("server stopped")
		}
		return value, nil
	}
	if err := c.BindLocalSource(source); err != nil {
		t.Fatal(err)
	}
	value = value.Add(time.Second)
	if got := c.Now(); !got.Equal(value) {
		t.Fatalf("local time: got %v, want %v", got, value)
	}
	if status := c.SourceStatus(); !status.Synchronized || !status.PTPEnabled {
		t.Fatalf("local binding is not synchronized: %+v", status)
	}
	available = false
	if err := c.LocalSourceError(); err == nil {
		t.Fatal("source loss was not reported")
	}
	afterLoss := reads
	available = true
	value = value.Add(time.Hour)
	got := c.Now()
	if got.Year() != 2020 || got.Sub(value.Add(-time.Hour)) > time.Second {
		t.Fatalf("local loss switched clocks: %v", got)
	}
	if status := c.SourceStatus(); status.Synchronized || !status.PTPEnabled {
		t.Fatalf("holdover must remain local and unsynchronized: %+v", status)
	}
	if reads != afterLoss {
		t.Fatal("source resumed automatically after loss")
	}
	if err := c.BindLocalSource(source); err != nil || !c.Now().Equal(value) {
		t.Fatalf("explicit rebind failed: %v", err)
	}
	c.DisablePTP()
	if c.SourceStatus().PTPEnabled || c.Now().Year() == 2020 {
		t.Fatal("explicit stop did not release the local clock")
	}
}

func TestLocalSourceRequiresValidTime(t *testing.T) {
	c := NewApplicationClock(0)
	for _, source := range []func() (time.Time, error){nil,
		func() (time.Time, error) { return time.Time{}, nil },
		func() (time.Time, error) { return time.Time{}, errors.New("unavailable") },
	} {
		if err := c.BindLocalSource(source); err == nil {
			t.Fatal("invalid local source accepted")
		}
		if c.SourceStatus().PTPEnabled {
			t.Fatal("failed binding modified the active clock")
		}
	}
}

func TestApplicationClockFallsBackToSystemWhenDisabled(t *testing.T) {
	c := NewApplicationClock(500_000)
	systemTime := time.Now().Add(-3 * time.Second)
	if delta := c.FromSystem(systemTime).Sub(systemTime); delta != 0 {
		t.Fatalf("disabled clock changed system timestamp by %v", delta)
	}
	if status := c.SourceStatus(); status.PTPEnabled || status.Synchronized {
		t.Fatalf("unexpected disabled status: %+v", status)
	}
}

func TestApplicationClockMapsSystemTimestampsAfterStep(t *testing.T) {
	c := NewApplicationClock(500_000)
	c.EnablePTP()
	if err := c.Step(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	systemTime := time.Now()
	got := c.FromSystem(systemTime)
	if delta := got.Sub(systemTime); delta < 1990*time.Millisecond || delta > 2010*time.Millisecond {
		t.Fatalf("mapped offset: want about 2s, got %v", delta)
	}

	c.DisablePTP()
	if delta := c.FromSystem(systemTime).Sub(systemTime); delta != 0 {
		t.Fatalf("fallback changed system timestamp by %v", delta)
	}
}

func TestApplicationClockReportsLockAndFrequency(t *testing.T) {
	c := NewApplicationClock(100)
	c.EnablePTP()
	c.SetSynchronized(true)
	if err := c.AdjustFrequency(250); err != nil {
		t.Fatal(err)
	}
	status := c.SourceStatus()
	if !status.PTPEnabled || !status.Synchronized {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.FrequencyPPB != 100 {
		t.Fatalf("frequency must be clamped to 100 ppb, got %v", status.FrequencyPPB)
	}
}
