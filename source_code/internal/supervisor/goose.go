package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/infrastructure/capture"
	"pbmt/internal/infrastructure/inject"
)

// GooseSubscriberSnapshots exposes accepted receiver state independently of
// presentation event subscriptions. Opening a view never starts/stops capture.
func (m *Manager) GooseSubscriberSnapshots(name string) ([]goosesub.Snapshot, ModuleStatus) {
	m.mu.Lock()
	rt := m.modules[ModuleGooseSub]
	status := ModuleStatus{ID: ModuleGooseSub, State: StatusStopped}
	var svc *goosesub.Service
	if rt != nil {
		status.State, status.Error, svc = rt.status, rt.err, rt.gooseSub
	}
	m.mu.Unlock()
	if svc == nil {
		return nil, status
	}
	return svc.Snapshots(name, time.Now()), status
}

func (m *Manager) startGoosePub(cfg *config.Config) (*moduleRuntime, error) {
	if !cfg.GoosePub.Enabled {
		return nil, errors.New("goose_pub is disabled in profile")
	}
	inj, err := inject.Open(inject.Config{Interface: cfg.GoosePub.Interface})
	if err != nil {
		return nil, err
	}

	rt := &moduleRuntime{
		status:    StatusRunning,
		close:     inj.Close,
		goosePubs: make(map[string]*goosepub.Service),
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel

	for i := range cfg.GoosePub.Publishers {
		pubCfg, err := buildGoosePublisherConfig(&cfg.GoosePub.Publishers[i])
		if err != nil {
			_ = inj.Close()
			cancel()
			return nil, err
		}
		svc := goosepub.New(pubCfg, inj, m.log.With("goose_pub", pubCfg.Name), m.clock)
		rt.goosePubs[pubCfg.Name] = svc

		rt.wg.Add(1)
		go func() {
			defer rt.wg.Done()
			if err := m.markRuntimeExit(ModuleGoosePub, rt, svc.Run(ctx), "stream", pubCfg.Name); err != nil {
				m.emit(Event{Module: ModuleGoosePub, Severity: "error", Message: err.Error()})
			}
		}()
	}
	return rt, nil
}

func (m *Manager) startGooseSub(cfg *config.Config) (*moduleRuntime, error) {
	if !cfg.GooseSub.Enabled {
		return nil, errors.New("goose_sub is disabled in profile")
	}
	subs, err := buildGooseSubscriptions(cfg.GooseSub.Subscriptions)
	if err != nil {
		return nil, err
	}
	src, err := capture.Open(capture.Config{
		Interface:   cfg.GooseSub.Interface,
		BPFFilter:   "ether proto 0x88b8 or (vlan and ether proto 0x88b8)",
		Promiscuous: cfg.GooseSub.Promiscuous,
		Immediate:   true,
	})
	if err != nil {
		return nil, err
	}

	rt := &moduleRuntime{status: StatusRunning, close: src.Close}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	svc := goosesub.New(src, subs, gooseEventSink{manager: m}, m.log.With("goose_sub", cfg.GooseSub.Interface))
	rt.gooseSub = svc
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := m.markRuntimeExit(ModuleGooseSub, rt, svc.Run(ctx)); err != nil {
			m.emit(Event{Module: ModuleGooseSub, Severity: "error", Message: err.Error()})
		}
	}()
	return rt, nil
}

func buildGoosePublisherConfig(c *config.GoosePublisher) (goosepub.PublisherConfig, error) {
	dstMAC, err := net.ParseMAC(c.DstMAC)
	if err != nil {
		return goosepub.PublisherConfig{}, fmt.Errorf("%s: dst_mac: %w", c.Name, err)
	}
	srcMAC, err := net.ParseMAC(c.SrcMAC)
	if err != nil {
		return goosepub.PublisherConfig{}, fmt.Errorf("%s: src_mac: %w", c.Name, err)
	}
	vlan := buildVLAN(c.VLANID, c.VLANPri)
	data, err := buildGooseDatasetInitialData(c.Dataset)
	if err != nil {
		return goosepub.PublisherConfig{}, fmt.Errorf("%s: dataset: %w", c.Name, err)
	}
	return goosepub.PublisherConfig{
		Name:        c.Name,
		DstMAC:      dstMAC,
		SrcMAC:      srcMAC,
		AppID:       uint16(c.AppID),
		GocbRef:     c.GocbRef,
		DatSet:      c.DatSet,
		GoID:        c.GoID,
		ConfRev:     c.ConfRev,
		NdsCom:      c.NdsCom,
		VLAN:        vlan,
		MinMs:       c.MinIntervalMs,
		MaxMs:       c.MaxIntervalMs,
		InitialData: data,
	}, nil
}

func buildGooseSubscriptions(cfgSubs []config.GooseSubscriber) ([]*goose.Subscription, error) {
	out := make([]*goose.Subscription, 0, len(cfgSubs))
	for i := range cfgSubs {
		c := &cfgSubs[i]
		dst, err := net.ParseMAC(c.DstMAC)
		if err != nil {
			return nil, fmt.Errorf("%s: dst_mac: %w", c.Name, err)
		}
		var src net.HardwareAddr
		if c.SrcMAC != "" {
			src, err = net.ParseMAC(c.SrcMAC)
			if err != nil {
				return nil, fmt.Errorf("%s: src_mac: %w", c.Name, err)
			}
		}
		out = append(out, &goose.Subscription{
			Name:             c.Name,
			DstMAC:           dst,
			SrcMAC:           src,
			AppID:            uint16(c.AppID),
			MatchAnyAppID:    c.MatchAnyAppID,
			GocbRef:          c.GocbRef,
			DatSet:           c.DatSet,
			GoID:             c.GoID,
			ConfRev:          c.ConfRev,
			VLANID:           c.VLANID,
			VLANPriority:     c.VLANPri,
			AcceptTest:       c.AcceptTest,
			AcceptSimulation: c.AcceptSimulation,
			AcceptNdsCom:     c.AcceptNdsCom,
		})
	}
	return out, nil
}

func buildGooseDatasetInitialData(entries []config.GooseDatasetEntry) ([]goose.DataValue, error) {
	out := make([]goose.DataValue, 0, len(entries))
	for i, entry := range entries {
		dv, err := gooseDatasetEntryDefaultValue(entry)
		if err != nil {
			return nil, fmt.Errorf("item #%d %q: %w", i, entry.Name, err)
		}
		out = append(out, dv)
	}
	return out, nil
}

func gooseDatasetEntryDefaultValue(entry config.GooseDatasetEntry) (goose.DataValue, error) {
	switch entry.Type {
	case "bool":
		return goose.DataValue{Type: goose.DataTypeBoolean, Bool: false}, nil
	case "quality":
		return goose.NewQualityBitString(0), nil
	case "int":
		return goose.DataValue{Type: goose.DataTypeInteger, Int: 0}, nil
	case "uint":
		return goose.DataValue{Type: goose.DataTypeUnsigned, UInt: 0}, nil
	case "float":
		return goose.DataValue{Type: goose.DataTypeFloatingPoint, Float: 0}, nil
	case "string":
		return goose.DataValue{Type: goose.DataTypeVisibleString, String: ""}, nil
	case "utc_time":
		return goose.DataValue{Type: goose.DataTypeUTCTime}, nil
	default:
		return goose.DataValue{}, fmt.Errorf("unsupported type %q", entry.Type)
	}
}

type gooseEventSink struct {
	manager *Manager
}

func (s gooseEventSink) OnGooseEvent(e goosesub.Event) {
	msg := fmt.Sprintf("%s subscription=%s", e.Reason, e.Subscription)
	if e.PDU != nil {
		msg += fmt.Sprintf(" appID=0x%04X stNum=%d sqNum=%d confRev=%d", e.PDU.AppID, e.PDU.StNum, e.PDU.SqNum, e.PDU.ConfRev)
	}
	severity := "info"
	if e.Reason == goosesub.ReasonStreamLost {
		severity = "warn"
	}
	level := slog.LevelInfo
	switch e.Reason {
	case goosesub.ReasonStreamLost, goosesub.ReasonPublisherRestart, goosesub.ReasonConfRevChanged:
		level = slog.LevelWarn
		severity = "warn"
	case goosesub.ReasonStateChange:
		level = slog.LevelDebug
	}
	// Log diagnostics, never the PDU/allData values. This does not depend on
	// a Manual view subscribing to presentation events.
	s.manager.log.Log(context.Background(), level, "GOOSE stream event",
		"module", ModuleGooseSub, "subscription", e.Subscription,
		"event", e.Reason, "event_time", e.ReceivedAt, "details", msg)
	s.manager.emit(Event{
		Module:       ModuleGooseSub,
		Time:         e.ReceivedAt,
		Message:      msg,
		Severity:     severity,
		Subscription: e.Subscription,
		GoosePDU:     e.PDU,
	})
}
