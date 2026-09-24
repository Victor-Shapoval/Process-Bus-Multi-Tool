package supervisor

import (
	"errors"
	"fmt"
	"time"

	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/svpub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

var errAgentOwnsControl = errors.New("manual control is locked while MCP is running")

func (m *Manager) GoosePublisherSnapshot(name string) (goosepub.Snapshot, bool) {
	m.mu.Lock()
	rt := m.modules[ModuleGoosePub]
	var svc *goosepub.Service
	if rt != nil {
		svc = rt.goosePubs[name]
	}
	m.mu.Unlock()
	if svc == nil {
		return goosepub.Snapshot{}, false
	}
	return svc.Snapshot(), true
}

func (m *Manager) AgentActive() bool {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	return m.agentActive
}

func (m *Manager) Start(id ModuleID) error {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		return errAgentOwnsControl
	}
	return m.start(id)
}

func (m *Manager) Stop(id ModuleID) error {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		return errAgentOwnsControl
	}
	return m.stop(id)
}

func (m *Manager) UpdateConfig(cfg *config.Config) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		m.log.Warn("configuration update rejected", "error", errAgentOwnsControl)
		return
	}
	m.updateConfig(cfg)
}

func (m *Manager) Close() error {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	m.agentActive = false
	m.agentEpoch++
	return m.close()
}

func (m *Manager) ApplyGoosePublisherState(name string, data []goose.DataValue, test, simulation bool) (bool, error) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		return false, errAgentOwnsControl
	}
	return m.applyGoosePublisherState(name, data, test, simulation)
}

func (m *Manager) SetSVPublisherWaveform(name string, values [sv.NumChannels]svpub.ChannelSetting, simulation bool, synch sv.SmpSynch) error {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		return errAgentOwnsControl
	}
	return m.setSVPublisherWaveform(name, values, simulation, synch)
}

type agentLease struct {
	manager *Manager
	epoch   uint64
}

func (m *Manager) AcquireAgent() (control.Agent, error) {
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if m.agentActive {
		return nil, errors.New("MCP already owns this project")
	}
	m.mu.Lock()
	unavailable := m.closed || m.cfg == nil
	m.mu.Unlock()
	if unavailable {
		return nil, errors.New("project runtime is unavailable")
	}
	m.agentEpoch++
	m.agentActive = true
	m.log.Info("control transferred to MCP")
	return &agentLease{m, m.agentEpoch}, nil
}

func (a *agentLease) valid() error {
	if !a.manager.agentActive || a.epoch != a.manager.agentEpoch {
		return errors.New("MCP control session has ended")
	}
	return nil
}

func (a *agentLease) Close() error {
	return a.release(true)
}

func (a *agentLease) Release() error {
	return a.release(false)
}

func (a *agentLease) release(stopPublishers bool) error {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if a.valid() != nil {
		return nil
	}
	// Fence in-flight/queued commands before transferring ownership.
	var first, second error
	if stopPublishers {
		first = m.stop(ModuleGoosePub)
		second = m.stop(ModuleSVPub)
	}
	m.agentActive = false
	m.agentEpoch++
	m.log.Info("MCP control released", "publishers_stopped", stopPublishers)
	return errors.Join(first, second)
}

func (a *agentLease) Statuses() ([]ModuleStatus, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return nil, err
	}
	var out []ModuleStatus
	for _, id := range []ModuleID{ModuleGooseSub, ModuleGoosePub, ModuleSVSub, ModuleSVPub} {
		out = append(out, m.Status(id))
	}
	return out, nil
}

func (a *agentLease) GooseInput(name string) ([]goosesub.Snapshot, ModuleStatus, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return nil, ModuleStatus{}, err
	}
	status := m.Status(ModuleGooseSub)
	m.mu.Lock()
	rt := m.modules[ModuleGooseSub]
	m.mu.Unlock()
	if rt == nil || rt.gooseSub == nil {
		return nil, status, nil
	}
	return rt.gooseSub.Snapshots(name, time.Now()), status, nil
}

func (a *agentLease) SVInput(name string) ([]svsub.Snapshot, ModuleStatus, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return nil, ModuleStatus{}, err
	}
	status := m.Status(ModuleSVSub)
	m.mu.Lock()
	rt := m.modules[ModuleSVSub]
	m.mu.Unlock()
	if rt == nil || rt.svSub == nil {
		return nil, status, nil
	}
	return rt.svSub.Snapshots(name, time.Now()), status, nil
}

func (a *agentLease) GooseOutput(name string) (goosepub.Snapshot, ModuleStatus, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return goosepub.Snapshot{}, ModuleStatus{}, err
	}
	status := m.Status(ModuleGoosePub)
	m.mu.Lock()
	rt := m.modules[ModuleGoosePub]
	m.mu.Unlock()
	if rt == nil || rt.goosePubs[name] == nil {
		return goosepub.Snapshot{}, status, fmt.Errorf("publisher %q has no runtime state; start it in Manual before MCP", name)
	}
	return rt.goosePubs[name].Snapshot(), status, nil
}

func (a *agentLease) SVOutput(name string) (svpub.Snapshot, ModuleStatus, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return svpub.Snapshot{}, ModuleStatus{}, err
	}
	status := m.Status(ModuleSVPub)
	m.mu.Lock()
	rt := m.modules[ModuleSVPub]
	m.mu.Unlock()
	if rt == nil || rt.svStreams[name] == nil {
		return svpub.Snapshot{}, status, fmt.Errorf("publisher %q has no runtime state; start it in Manual before MCP", name)
	}
	return rt.svStreams[name].Snapshot(), status, nil
}

func (a *agentLease) ApplyGoose(name string, data []goose.DataValue, test, simulation bool) (bool, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return false, err
	}
	return m.applyGoosePublisherState(name, data, test, simulation)
}

func (a *agentLease) ApplySV(name string, values [sv.NumChannels]svpub.ChannelSetting, simulation bool, synch sv.SmpSynch) error {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return err
	}
	return m.setSVPublisherWaveform(name, values, simulation, synch)
}

func (a *agentLease) ObserveGoose(name string) (*goosesub.Watch, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.modules[ModuleGooseSub]
	if rt == nil || rt.status != StatusRunning || rt.gooseSub == nil {
		return nil, errors.New("GOOSE subscriber is not running")
	}
	return rt.gooseSub.Observe(name), nil
}

func (a *agentLease) ApplySVTracked(name string, values [sv.NumChannels]svpub.ChannelSetting, simulation bool, synch sv.SmpSynch) (<-chan svpub.SendReceipt, error) {
	m := a.manager
	m.commandMu.Lock()
	defer m.commandMu.Unlock()
	if err := a.valid(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	rt := m.modules[ModuleSVPub]
	if rt == nil || rt.status != StatusRunning || rt.svStreams[name] == nil {
		m.mu.Unlock()
		return nil, errors.New("SV publisher is not running")
	}
	svc := rt.svStreams[name]
	m.mu.Unlock()
	return svc.SetWaveformTracked(values, simulation, synch)
}
