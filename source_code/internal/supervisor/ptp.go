package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"pbmt/internal/application/ptpclient"
	"pbmt/internal/application/ptpport"
	"pbmt/internal/application/ptpserver"
	"pbmt/internal/config"
	ptpdomain "pbmt/internal/domain/ptp"
	ptptransport "pbmt/internal/infrastructure/ptp"
)

func resolvePTPClockIdentity(ifaceName string, clientRole bool) (ptpdomain.ClockIdentity, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return ptpdomain.ClockIdentity{}, fmt.Errorf("PTP interface %q: %w", ifaceName, err)
	}
	if len(iface.HardwareAddr) < 6 {
		return ptpdomain.ClockIdentity{}, fmt.Errorf("PTP interface %q has no MAC address", ifaceName)
	}
	var mac [6]byte
	copy(mac[:], iface.HardwareAddr[:6])
	if clientRole {
		return ptpdomain.ClientClockIdentityFromMAC(mac), nil
	}
	return ptpdomain.ClockIdentityFromMAC(mac), nil
}

func configuredPTPDelayMechanism(value string) uint8 {
	switch value {
	case "e2e":
		return ptpdomain.DelayMechanismE2E
	case "p2p":
		return ptpdomain.DelayMechanismP2P
	default:
		return 0
	}
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

func (m *Manager) startPTPClient(cfg *config.Config) (*moduleRuntime, error) {
	c := &cfg.PTPClient
	if !c.Enabled {
		return nil, errors.New("ptp_client is disabled in profile")
	}
	if c.Source == "local" {
		return m.startLocalPTPClient()
	}
	if c.Source != "" && c.Source != "external" {
		return nil, fmt.Errorf("ptp_client: unsupported source %q", c.Source)
	}

	profile := ptpdomain.DefaultProfile
	if c.Profile == "power" {
		profile = ptpdomain.PowerProfile
		profile.C37238Version, _ = ptpdomain.ParseC37238Version(c.PowerProfile.Version)
		if c.Transport != "ethernet" {
			return nil, errors.New("ptp_client: power profile requires ethernet transport")
		}
	}
	delayMechanism := configuredPTPDelayMechanism(c.DelayMechanism)
	transportType := c.Transport
	if transportType == "" {
		transportType = "udp"
	}
	tr, err := ptptransport.New(c.Interface, transportType, "auto")
	if err != nil {
		return nil, err
	}
	identity, err := resolvePTPClockIdentity(c.Interface, true)
	if err != nil {
		tr.Close()
		return nil, err
	}
	selfID := ptpdomain.PortIdentity{ClockIdentity: identity, PortNumber: 1}
	clientCfg := ptpclient.Config{
		Profile:        profile,
		Transport:      transportType,
		TimestampMode:  tr.TimestampMode(),
		DomainNumber:   c.DomainNumber,
		DelayMechanism: delayMechanism,
	}
	client := ptpclient.New(tr, m.clock, selfID, clientCfg, m.log.With("ptp_client", c.Interface))
	m.log.Info("PTP transport ready", "module", ModulePTPClient, "interface", c.Interface, "transport", transportType, "timestamp_mode", tr.TimestampMode())
	rt := &moduleRuntime{status: StatusRunning, ptpClient: client}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	m.clock.EnablePTP()
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		runErr := client.Run(ctx)
		m.clock.DisablePTP()
		if err := m.markRuntimeExit(ModulePTPClient, rt, runErr); err != nil {
			m.emit(Event{Module: ModulePTPClient, Severity: "error", Message: err.Error()})
		}
	}()
	m.emit(Event{
		Module:   ModulePTPClient,
		Severity: "info",
		Message: fmt.Sprintf("started slave-only clock on %s profile=%s delay=%s domain=%d timestamp=%s",
			c.Interface, profile.Name, client.Snapshot().DelayMechanism,
			client.Snapshot().DomainNumber, tr.TimestampMode()),
	})
	return rt, nil
}

func (m *Manager) startPTPServer(cfg *config.Config) (*moduleRuntime, error) {
	c := &cfg.PTPServer
	if !c.Enabled {
		return nil, errors.New("ptp_server is disabled in profile")
	}

	profile := ptpdomain.DefaultProfile
	c37Version := ptpdomain.C37238Disabled
	grandmasterID := uint16(3)
	grandmasterTimeInaccuracy := uint32(60)
	networkTimeInaccuracy := uint32(0)
	totalTimeInaccuracy := uint32(100)
	var alternateTimeOffset *ptpdomain.AlternateTimeOffsetTLV
	if c.Profile == "power" {
		profile = ptpdomain.PowerProfile
		if c.Transport != "ethernet" {
			return nil, errors.New("ptp_server: power profile requires ethernet transport")
		}
		c37Version, _ = ptpdomain.ParseC37238Version(c.PowerProfile.Version)
		grandmasterID = valueOr(c.PowerProfile.GrandmasterID, grandmasterID)
		grandmasterTimeInaccuracy = valueOr(c.PowerProfile.GrandmasterTimeInaccuracy, grandmasterTimeInaccuracy)
		networkTimeInaccuracy = valueOr(c.PowerProfile.NetworkTimeInaccuracy, networkTimeInaccuracy)
		totalTimeInaccuracy = valueOr(c.PowerProfile.TotalTimeInaccuracy, totalTimeInaccuracy)
		alternate := &c.PowerProfile.AlternateTimeOffset
		if valueOr(alternate.Enabled, true) {
			displayName := alternate.DisplayName
			if displayName == "" {
				displayName = "UTC+03:00"
			}
			alternateTimeOffset = &ptpdomain.AlternateTimeOffsetTLV{
				KeyField:       valueOr(alternate.KeyField, uint8(1)),
				CurrentOffset:  valueOr(alternate.CurrentOffset, int32(10763)),
				JumpSeconds:    valueOr(alternate.JumpSeconds, int32(0)),
				TimeOfNextJump: valueOr(alternate.TimeOfNextJump, uint64(0)),
				DisplayName:    displayName,
			}
		}
	}
	transportType := c.Transport
	if transportType == "" {
		transportType = "udp"
	}
	tr, err := ptptransport.NewGrandmaster(c.Interface, transportType)
	if err != nil {
		return nil, err
	}

	identity, err := resolvePTPClockIdentity(c.Interface, false)
	if err != nil {
		tr.Close()
		return nil, err
	}
	selfID := ptpdomain.PortIdentity{
		ClockIdentity: identity,
		PortNumber:    1,
	}

	utcOffset := int16(37)
	if c.UTCOffset != nil {
		utcOffset = *c.UTCOffset
	}
	timeSource, ok := config.ParseTimeSource(c.TimeSource)
	if !ok {
		tr.Close()
		return nil, fmt.Errorf("ptp_server: unknown time source %q", c.TimeSource)
	}
	serviceCfg := ptpserver.Config{
		Profile:                   profile,
		Transport:                 transportType,
		DomainNumber:              c.DomainNumber,
		DelayMechanism:            configuredPTPDelayMechanism(c.DelayMechanism),
		UTCOffset:                 utcOffset,
		TimeSource:                timeSource,
		Priority1:                 c.Priority1,
		Priority2:                 c.Priority2,
		ClockClass:                c.ClockClass,
		ClockAccuracy:             c.ClockAccuracy,
		ClockVariance:             c.ClockVariance,
		TimeTraceable:             c.TimeTraceable,
		FrequencyTraceable:        c.FrequencyTraceable,
		C37238Version:             c37Version,
		C37238GrandmasterID:       grandmasterID,
		GrandmasterTimeInaccuracy: grandmasterTimeInaccuracy,
		NetworkTimeInaccuracy:     networkTimeInaccuracy,
		TotalTimeInaccuracy:       totalTimeInaccuracy,
		AlternateTimeOffset:       alternateTimeOffset,
	}
	server := ptpserver.New(tr, selfID, serviceCfg, m.log.With("ptp_server", c.Interface))
	m.log.Info("PTP transport ready", "module", ModulePTPServer, "interface", c.Interface, "transport", transportType, "timestamp_mode", "hardware")
	rt := &moduleRuntime{status: StatusRunning, ptpServer: server}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	rt.ptpTime = func() (time.Time, error) {
		if ctx.Err() != nil || server.State() != ptpserver.StateMaster {
			return time.Time{}, errors.New("local PTP Server is not running")
		}
		source, ok := tr.(ptpport.TimeSource)
		if !ok {
			return time.Time{}, errors.New("local PTP Server clock is unavailable")
		}
		return source.Now()
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := m.markRuntimeExit(ModulePTPServer, rt, server.Run(ctx)); err != nil {
			m.emit(Event{Module: ModulePTPServer, Severity: "error", Message: err.Error()})
		}
	}()
	m.emit(Event{
		Module:   ModulePTPServer,
		Severity: "info",
		Message: fmt.Sprintf("started forced Grandmaster on %s profile=%s delay=%s domain=%d timestamp=%s",
			c.Interface, profile.Name, server.Snapshot().DelayMechanism,
			server.Snapshot().DomainNumber, "hardware"),
	})
	return rt, nil
}

func (m *Manager) startLocalPTPClient() (*moduleRuntime, error) {
	m.mu.Lock()
	server := m.modules[ModulePTPServer]
	if server == nil || server.status != StatusRunning || server.ptpTime == nil || server.ptpServer == nil {
		m.mu.Unlock()
		return nil, errors.New("ptp_client: Local source requires a running PTP Server; start it first")
	}
	source := server.ptpTime
	serverSnapshot := server.ptpServer.Snapshot
	m.mu.Unlock()
	if err := m.clock.BindLocalSource(source); err != nil {
		return nil, fmt.Errorf("ptp_client: bind Local source: %w", err)
	}
	rt := &moduleRuntime{status: StatusRunning}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	rt.localPTPSnapshot = func() ptpclient.Snapshot {
		gm := serverSnapshot()
		applicationTime := m.clock.Now()
		status := m.clock.SourceStatus()
		state := ptpclient.StateFaulty
		if status.Synchronized {
			state = ptpclient.StateSlave
		}
		return ptpclient.Snapshot{
			State: state, Source: "local", Profile: gm.Profile,
			DomainNumber: gm.DomainNumber, Transport: "internal", TimestampMode: "local",
			GrandmasterIdentity: gm.GrandmasterClockIdentity,
			MasterPortIdentity:  ptpdomain.PortIdentity{ClockIdentity: gm.GrandmasterClockIdentity, PortNumber: gm.PortNumber},
			UTCOffset:           gm.CurrentUTCOffset, ClockStatus: status, ApplicationTime: applicationTime,
		}
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.clock.LocalSourceError(); err != nil {
					failure := fmt.Errorf("ptp_client: Local source lost; no automatic fallback: %w", err)
					if err := m.markRuntimeExit(ModulePTPClient, rt, failure); err != nil {
						m.emit(Event{Module: ModulePTPClient, Severity: "error", Message: err.Error()})
					}
					return
				}
			}
		}
	}()
	m.log.Info("PTP client bound to local Server clock", "module", ModulePTPClient, "source", "local")
	m.emit(Event{Module: ModulePTPClient, Severity: "info", Message: "Local PTP Server clock selected; no network exchange or automatic fallback"})
	return rt, nil
}
