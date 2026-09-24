package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/ptpclient"
	"pbmt/internal/application/ptpserver"
	"pbmt/internal/application/svpub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/config"
	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

type ModuleID = control.ModuleID
type Status = control.Status
type ModuleStatus = control.ModuleStatus
type Event = control.Event

const (
	ModuleGooseSub  = control.ModuleGooseSub
	ModuleGoosePub  = control.ModuleGoosePub
	ModuleSVSub     = control.ModuleSVSub
	ModuleSVPub     = control.ModuleSVPub
	ModulePTPClient = control.ModulePTPClient
	ModulePTPServer = control.ModulePTPServer
	StatusStopped   = control.StatusStopped
	StatusRunning   = control.StatusRunning
	StatusError     = control.StatusError
)

var _ control.Controller = (*Manager)(nil)

type Manager struct {
	cfg *config.Config
	log *slog.Logger

	mu          sync.Mutex
	modules     map[ModuleID]*moduleRuntime
	starting    map[ModuleID]chan struct{}
	updateDone  chan struct{}
	subscribers map[chan Event]struct{}
	startModule func(ModuleID, *config.Config) (*moduleRuntime, error)
	clock       *appclock.ApplicationClock
	closed      bool
	commandMu   sync.Mutex
	agentActive bool
	agentEpoch  uint64
}

type moduleRuntime struct {
	status Status
	err    string

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	close    func() error
	stopOnce sync.Once
	stopErr  error
	// Guarded by Manager.mu; lets project shutdown also drain cleanup diagnostics.
	cleanupDone chan struct{}

	goosePubs        map[string]*goosepub.Service
	svStreams        map[string]*svpub.Service
	ptpServer        *ptpserver.Service
	ptpClient        *ptpclient.Service
	localPTPSnapshot func() ptpclient.Snapshot
	ptpTime          func() (time.Time, error)
	gooseSub         *goosesub.Service
	svSub            *svsub.Service
}

func (rt *moduleRuntime) shutdown() error {
	if rt == nil {
		return nil
	}
	rt.stopOnce.Do(func() {
		if rt.cancel != nil {
			rt.cancel()
		}
		if rt.close != nil {
			rt.stopErr = rt.close()
		}
		rt.wg.Wait()
	})
	return rt.stopErr
}

func New(cfg *config.Config, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{
		cfg:         cfg,
		log:         log,
		modules:     make(map[ModuleID]*moduleRuntime),
		starting:    make(map[ModuleID]chan struct{}),
		subscribers: make(map[chan Event]struct{}),
		clock:       appclock.NewApplicationClock(500_000),
	}
	m.startModule = m.startConfiguredModule
	return m
}

func (m *Manager) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	m.mu.Lock()
	if m.closed {
		close(ch)
		m.mu.Unlock()
		return ch, func() {}
	}
	m.subscribers[ch] = struct{}{}
	m.mu.Unlock()
	cancel := func() {
		m.mu.Lock()
		if _, ok := m.subscribers[ch]; ok {
			delete(m.subscribers, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
	return ch, cancel
}

func (m *Manager) updateConfig(cfg *config.Config) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		if done := m.updateDone; done != nil {
			m.mu.Unlock()
			<-done
			continue
		}
		if len(m.starting) != 0 {
			pending := make([]chan struct{}, 0, len(m.starting))
			for _, done := range m.starting {
				pending = append(pending, done)
			}
			m.mu.Unlock()
			for _, done := range pending {
				<-done
			}
			continue
		}
		done := make(chan struct{})
		m.updateDone = done
		type stoppingRuntime struct {
			id      ModuleID
			rt      *moduleRuntime
			changed bool
			cleanup <-chan struct{}
		}
		runtimes := make([]stoppingRuntime, 0, len(m.modules))
		for id, rt := range m.modules {
			if rt == nil {
				continue
			}
			runtimes = append(runtimes, stoppingRuntime{id, rt, rt.status != StatusStopped, rt.cleanupDone})
			rt.status = StatusStopped
		}
		m.mu.Unlock()

		for _, stop := range runtimes {
			err := stop.rt.shutdown()
			if stop.cleanup != nil {
				<-stop.cleanup
			}
			if err != nil {
				m.log.Error("module stop failed", "module", stop.id, "reason", "config_update", "error", err)
			} else if stop.changed {
				m.log.Info("module stopped", "module", stop.id, "state", StatusStopped, "reason", "config_update")
			}
		}
		m.clock.DisablePTP()

		m.mu.Lock()
		if !m.closed {
			m.cfg = cfg
		}
		m.updateDone = nil
		close(done)
		m.mu.Unlock()
		return
	}
}

func (m *Manager) Status(id ModuleID) ModuleStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.modules[id]
	if rt == nil {
		return ModuleStatus{ID: id, State: StatusStopped}
	}
	return ModuleStatus{ID: id, State: rt.status, Error: rt.err}
}

func (m *Manager) start(id ModuleID) error {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			m.log.Error("module start failed", "module", id, "error", "supervisor is closed")
			return errors.New("supervisor is closed")
		}
		if done := m.updateDone; done != nil {
			m.mu.Unlock()
			<-done
			continue
		}
		if rt := m.modules[id]; rt != nil && rt.status == StatusRunning {
			m.mu.Unlock()
			return nil
		}
		if done := m.starting[id]; done != nil {
			m.mu.Unlock()
			<-done
			status := m.Status(id)
			if status.State == StatusRunning {
				return nil
			}
			if status.State == StatusError {
				return errors.New(status.Error)
			}
			continue
		}
		done := make(chan struct{})
		m.starting[id] = done
		oldRuntime := m.modules[id]
		var oldCleanup chan struct{}
		if oldRuntime != nil {
			oldCleanup = oldRuntime.cleanupDone
		}
		cfg := m.cfg
		m.mu.Unlock()

		defer func() {
			m.mu.Lock()
			delete(m.starting, id)
			close(done)
			m.mu.Unlock()
		}()

		if oldRuntime != nil {
			if err := oldRuntime.shutdown(); err != nil {
				m.log.Warn("clean up previous module runtime", "module", id, "error", err)
			}
			if oldCleanup != nil {
				<-oldCleanup
			}
			m.mu.Lock()
			if m.modules[id] == oldRuntime {
				oldRuntime.status = StatusStopped
			}
			if m.closed {
				m.mu.Unlock()
				m.log.Error("module start failed", "module", id, "error", "supervisor is closed")
				return errors.New("supervisor is closed")
			}
			cfg = m.cfg
			m.mu.Unlock()
		}

		if cfg == nil {
			return m.setError(id, errors.New("runtime: config is not loaded"))
		}

		rt, err := m.startModule(id, cfg)
		if err != nil {
			return m.setError(id, err)
		}

		m.mu.Lock()
		m.modules[id] = rt
		statusErr := rt.err
		status := rt.status
		if status == StatusRunning {
			m.log.Info("module started", "module", id, "state", status)
		}
		m.mu.Unlock()
		if status == StatusError {
			return errors.New(statusErr)
		}
		return nil
	}
}

func (m *Manager) startConfiguredModule(id ModuleID, cfg *config.Config) (*moduleRuntime, error) {
	switch id {
	case ModuleGooseSub:
		return m.startGooseSub(cfg)
	case ModuleGoosePub:
		return m.startGoosePub(cfg)
	case ModuleSVSub:
		return m.startSVSub(cfg)
	case ModuleSVPub:
		return m.startSVPub(cfg)
	case ModulePTPClient:
		return m.startPTPClient(cfg)
	case ModulePTPServer:
		return m.startPTPServer(cfg)
	default:
		return nil, fmt.Errorf("runtime: start is not implemented for %s", id)
	}
}

// PTPClientSnapshot returns the PTP Client and internal clock state.
func (m *Manager) PTPClientSnapshot() (ptpclient.Snapshot, bool) {
	m.mu.Lock()
	rt := m.modules[ModulePTPClient]
	if rt != nil && rt.localPTPSnapshot != nil && rt.status != StatusStopped {
		snapshot := rt.localPTPSnapshot
		m.mu.Unlock()
		return snapshot(), true
	}
	if rt == nil || rt.ptpClient == nil || rt.status != StatusRunning {
		m.mu.Unlock()
		return ptpclient.Snapshot{}, false
	}
	client := rt.ptpClient
	m.mu.Unlock()
	return client.Snapshot(), true
}

// PTPServerSnapshot returns the effective parameters of the running Grandmaster.
func (m *Manager) PTPServerSnapshot() (ptpserver.Snapshot, bool) {
	m.mu.Lock()
	rt := m.modules[ModulePTPServer]
	if rt == nil || rt.ptpServer == nil || rt.status != StatusRunning {
		m.mu.Unlock()
		return ptpserver.Snapshot{}, false
	}
	server := rt.ptpServer
	m.mu.Unlock()
	return server.Snapshot(), true
}

func (m *Manager) emit(e Event) {
	if e.Time.IsZero() {
		e.Time = m.clock.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

func (m *Manager) stop(id ModuleID) error {
	m.mu.Lock()
	if done := m.starting[id]; done != nil {
		m.mu.Unlock()
		<-done
		return m.stop(id)
	}
	rt := m.modules[id]
	if rt == nil {
		m.mu.Unlock()
		return nil
	}
	previous := rt.status
	cleanup := rt.cleanupDone
	rt.status = StatusStopped
	m.mu.Unlock()
	err := rt.shutdown()
	if id == ModulePTPClient {
		m.clock.DisablePTP()
	}
	if cleanup != nil {
		<-cleanup
	}
	if previous != StatusStopped {
		if err != nil {
			m.log.Error("module stop failed", "module", id, "error", err)
		} else {
			m.log.Info("module stopped", "module", id, "state", StatusStopped, "previous_state", previous)
		}
	}
	return err
}

// Close stops every module and releases presentation subscriptions. It is
// idempotent and prevents subsequent module starts.
func (m *Manager) close() error {
	m.mu.Lock()
	alreadyClosed := m.closed
	m.closed = true
	m.mu.Unlock()
	if alreadyClosed {
		return nil
	}

	var stopErrors []error
	for _, id := range control.Modules() {
		if err := m.stop(id); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("stop %s: %w", id, err))
		}
	}
	m.clock.DisablePTP()

	m.mu.Lock()
	for ch := range m.subscribers {
		delete(m.subscribers, ch)
		close(ch)
	}
	m.mu.Unlock()
	return errors.Join(stopErrors...)
}

// ApplyGoosePublisherState atomically applies the Dataset and service flags.
// changed is true only if the published state actually changed.
func (m *Manager) applyGoosePublisherState(name string, data []goose.DataValue, test, simulation bool) (changed bool, err error) {
	defer func() {
		if err != nil {
			m.log.Error("publisher update failed", "module", ModuleGoosePub, "stream", name, "error", err)
		} else if changed {
			m.log.Info("publisher state applied", "module", ModuleGoosePub, "stream", name)
		}
	}()
	m.mu.Lock()
	rt := m.modules[ModuleGoosePub]
	if rt == nil || rt.status != StatusRunning {
		m.mu.Unlock()
		return false, errors.New("goose_pub is not running")
	}
	pub := rt.goosePubs[name]
	m.mu.Unlock()
	if pub == nil {
		return false, fmt.Errorf("goose publisher %q not found", name)
	}
	return pub.ApplyState(data, test, simulation), nil
}

func (m *Manager) setSVPublisherWaveform(name string, settings [sv.NumChannels]svpub.ChannelSetting, simulation bool, smpSynch sv.SmpSynch) (err error) {
	defer func() {
		if err != nil {
			m.log.Error("publisher update failed", "module", ModuleSVPub, "stream", name, "error", err)
		} else {
			m.log.Info("publisher waveform applied", "module", ModuleSVPub, "stream", name)
		}
	}()
	m.mu.Lock()
	rt := m.modules[ModuleSVPub]
	if rt == nil || rt.status != StatusRunning {
		m.mu.Unlock()
		return errors.New("sv_pub is not running")
	}
	stream := rt.svStreams[name]
	m.mu.Unlock()
	if stream == nil {
		return fmt.Errorf("sv stream %q not found", name)
	}
	return stream.SetWaveform(settings, simulation, smpSynch)
}

func (m *Manager) setError(id ModuleID, err error) error {
	m.mu.Lock()
	m.modules[id] = &moduleRuntime{status: StatusError, err: err.Error()}
	m.log.Error("module start failed", "module", id, "state", StatusError, "error", err)
	m.mu.Unlock()
	return err
}

func (m *Manager) markRuntimeExit(id ModuleID, rt *moduleRuntime, err error, fields ...any) error {
	m.mu.Lock()
	if rt.status != StatusRunning {
		m.mu.Unlock()
		return nil
	}
	if err == nil {
		err = fmt.Errorf("%s stopped unexpectedly", id)
	}
	rt.status = StatusError
	rt.err = err.Error()
	rt.cleanupDone = make(chan struct{})
	cleanupDone := rt.cleanupDone
	attrs := append([]any{"module", id, "state", StatusError, "error", err}, fields...)
	m.log.Error("module failed", attrs...)
	m.mu.Unlock()

	go func() {
		defer close(cleanupDone)
		if cleanupErr := rt.shutdown(); cleanupErr != nil {
			m.log.Warn("clean up failed module runtime", "module", id, "error", cleanupErr)
		}
	}()
	return err
}
