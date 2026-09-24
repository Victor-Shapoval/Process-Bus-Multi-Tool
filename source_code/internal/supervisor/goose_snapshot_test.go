package supervisor

import (
	"testing"

	"pbmt/internal/application/goosesub"
	"pbmt/internal/config"
)

func TestGooseSubscriberSnapshotWithoutRuntime(t *testing.T) {
	m := New(&config.Config{}, nil)
	if values, state := m.GooseSubscriberSnapshots("events"); len(values) != 0 || state.State != StatusStopped || state.ID != ModuleGooseSub {
		t.Fatal("unstarted subscriber returned values or incorrect status", values, state)
	}
	m.modules[ModuleGooseSub] = &moduleRuntime{status: StatusRunning, gooseSub: goosesub.New(nil, nil, nil, nil)}
	if values, state := m.GooseSubscriberSnapshots("events"); len(values) != 0 || state.State != StatusRunning {
		t.Fatal("empty running subscriber returned values or incorrect status", values, state)
	}
	m.modules[ModuleGooseSub].status = StatusError
	m.modules[ModuleGooseSub].err = "capture failed"
	if values, state := m.GooseSubscriberSnapshots("events"); len(values) != 0 || state.State != StatusError || state.Error != "capture failed" {
		t.Fatal("snapshot omitted module failure", values, state)
	}
}
