package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"pbmt/internal/application/svpub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/config"
	"pbmt/internal/domain/sv"
	"pbmt/internal/infrastructure/capture"
	"pbmt/internal/infrastructure/inject"
)

// SVPublisherSnapshot exposes send timing without changing module state.
func (m *Manager) SVPublisherSnapshot(name string) (svpub.Snapshot, bool) {
	m.mu.Lock()
	rt := m.modules[ModuleSVPub]
	var svc *svpub.Service
	if rt != nil {
		svc = rt.svStreams[name]
	}
	m.mu.Unlock()
	if svc == nil {
		return svpub.Snapshot{}, false
	}
	return svc.Snapshot(), true
}

func (m *Manager) startSVPub(cfg *config.Config) (*moduleRuntime, error) {
	if !cfg.SVPub.Enabled {
		return nil, errors.New("sv_pub is disabled in profile")
	}
	streamConfigs := make([]svpub.PublisherConfig, 0, len(cfg.SVPub.Streams))
	for i := range cfg.SVPub.Streams {
		streamCfg, err := buildSVPublisherConfig(&cfg.SVPub.Streams[i])
		if err != nil {
			return nil, err
		}
		streamConfigs = append(streamConfigs, streamCfg)
	}

	inj, err := inject.Open(inject.Config{Interface: cfg.SVPub.Interface})
	if err != nil {
		return nil, err
	}

	rt := &moduleRuntime{
		status:    StatusRunning,
		close:     inj.Close,
		svStreams: make(map[string]*svpub.Service),
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel

	for _, streamCfg := range streamConfigs {
		svc := svpub.New(streamCfg, inj, m.log.With("sv_pub", streamCfg.Name), m.clock)
		rt.svStreams[streamCfg.Name] = svc

		m.emit(Event{
			Module:   ModuleSVPub,
			Severity: "info",
			Message: fmt.Sprintf("started %s on %s dst=%s src=%s app_id=0x%04X sv_id=%s",
				streamCfg.Name,
				cfg.SVPub.Interface,
				streamCfg.DstMAC.String(),
				streamCfg.SrcMAC.String(),
				streamCfg.AppID,
				streamCfg.SvID,
			),
		})

		rt.wg.Add(1)
		go func(svc *svpub.Service, name string) {
			defer rt.wg.Done()
			if err := m.markRuntimeExit(ModuleSVPub, rt, svc.Run(ctx), "stream", name); err != nil {
				m.emit(Event{Module: ModuleSVPub, Severity: "error", Message: err.Error()})
			}
		}(svc, streamCfg.Name)
	}
	return rt, nil
}

func (m *Manager) startSVSub(cfg *config.Config) (*moduleRuntime, error) {
	if !cfg.SVSub.Enabled {
		return nil, errors.New("sv_sub is disabled in profile")
	}
	subs, err := buildSVSubscriptions(cfg.SVSub.Subscriptions)
	if err != nil {
		return nil, err
	}
	src, err := capture.Open(capture.Config{
		Interface:   cfg.SVSub.Interface,
		BPFFilter:   "ether proto 0x88ba or (vlan and ether proto 0x88ba)",
		Promiscuous: cfg.SVSub.Promiscuous,
		Immediate:   true,
	})
	if err != nil {
		return nil, err
	}

	rt := &moduleRuntime{status: StatusRunning, close: src.Close}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	svc := svsub.New(src, subs, svEventSink{manager: m}, m.log.With("sv_sub", cfg.SVSub.Interface))
	rt.svSub = svc
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		if err := m.markRuntimeExit(ModuleSVSub, rt, svc.Run(ctx)); err != nil {
			m.emit(Event{Module: ModuleSVSub, Severity: "error", Message: err.Error()})
		}
	}()
	return rt, nil
}

func buildSVPublisherConfig(c *config.SVPublisher) (svpub.PublisherConfig, error) {
	dstMAC, err := net.ParseMAC(c.DstMAC)
	if err != nil {
		return svpub.PublisherConfig{}, fmt.Errorf("%s: dst_mac: %w", c.Name, err)
	}
	srcMAC, err := net.ParseMAC(c.SrcMAC)
	if err != nil {
		return svpub.PublisherConfig{}, fmt.Errorf("%s: src_mac: %w", c.Name, err)
	}
	return svpub.PublisherConfig{
		Name:                  c.Name,
		DstMAC:                dstMAC,
		SrcMAC:                srcMAC,
		AppID:                 uint16(c.AppID),
		SvID:                  c.SvID,
		DatSet:                c.DatSet,
		ConfRev:               c.ConfRev,
		VLAN:                  buildVLAN(c.VLANID, c.VLANPri),
		SmpRate:               c.SmpRate,
		SampleTimingFrequency: c.SampleTimingFrequency,
		SmpSynch:              sv.SmpSynch(c.SmpSynch),
	}, nil
}

func buildSVSubscriptions(cfgSubs []config.SVSubscriber) ([]*sv.Subscription, error) {
	out := make([]*sv.Subscription, 0, len(cfgSubs))
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
		out = append(out, &sv.Subscription{
			Name:                  c.Name,
			DstMAC:                dst,
			SrcMAC:                src,
			AppID:                 uint16(c.AppID),
			MatchAnyAppID:         c.MatchAnyAppID,
			SvID:                  c.SvID,
			VLANID:                c.VLANID,
			SmpRate:               c.SmpRate,
			SampleTimingFrequency: c.SampleTimingFrequency,
			BaseVector:            c.BaseVector,
		})
	}
	return out, nil
}

type svEventSink struct {
	manager *Manager
}

func (s svEventSink) OnSVEvent(e svsub.Event) {
	msg := fmt.Sprintf("%s subscription=%s", e.Reason, e.Subscription)
	var values [sv.NumChannels]float64
	hasValues := false
	if e.ASDU != nil {
		msg += fmt.Sprintf(" appID=0x%04X svID=%s smpCnt=%d confRev=%d synch=%s", e.ASDU.AppID, e.ASDU.SvID, e.ASDU.SmpCnt, e.ASDU.ConfRev, e.ASDU.SmpSynch.String())
	}
	if e.Stats != nil {
		msg += fmt.Sprintf(" samples=%d gaps=%d missed=%d", e.Stats.SamplesReceived, e.Stats.GapEvents, e.Stats.MissedTotal)
		values = e.Stats.Instant
		hasValues = true
	}
	switch e.Reason {
	case svsub.ReasonSmpCntGap:
		msg += fmt.Sprintf(" expected=%d actual=%d missed=%d", e.ExpectedSmpCnt, e.ActualSmpCnt, e.MissedSamples)
	case svsub.ReasonConfRevChanged:
		msg += fmt.Sprintf(" previous_confRev=%d", e.PrevConfRev)
	case svsub.ReasonSyncChanged:
		msg += fmt.Sprintf(" previous_sync=%s", e.PrevSmpSynch.String())
	}
	severity := "info"
	switch e.Reason {
	case svsub.ReasonStreamLost, svsub.ReasonSmpCntGap, svsub.ReasonConfRevChanged, svsub.ReasonSyncChanged:
		severity = "warn"
	}
	// Periodic snapshots/statistics carry measurements and are GUI-only.
	if e.Reason != svsub.ReasonSnapshot && e.Reason != svsub.ReasonStats {
		level := slog.LevelInfo
		if severity == "warn" {
			level = slog.LevelWarn
		}
		s.manager.log.Log(context.Background(), level, "SV stream event",
			"module", ModuleSVSub, "subscription", e.Subscription,
			"event", e.Reason, "event_time", e.ReceivedAt, "details", msg)
	}
	s.manager.emit(Event{
		Module:       ModuleSVSub,
		Time:         e.ReceivedAt,
		Message:      msg,
		Severity:     severity,
		Subscription: e.Subscription,
		SVValues:     values,
		HasSVValues:  hasValues,
		SVRMS:        statsRMS(e.Stats),
		SVAngle:      statsAngle(e.Stats),
		SVQuality:    statsQuality(e.Stats),
		SVFrequency:  statsFrequency(e.Stats),
		SVSynch:      statsSynch(e.Stats),
		SVSimulation: statsSimulation(e.Stats),
	})
}

func statsRMS(st *svsub.StreamStats) [sv.NumChannels]float64 {
	if st == nil {
		return [sv.NumChannels]float64{}
	}
	return st.RMS
}

func statsAngle(st *svsub.StreamStats) [sv.NumChannels]float64 {
	if st == nil {
		return [sv.NumChannels]float64{}
	}
	return st.Angle
}

func statsQuality(st *svsub.StreamStats) [sv.NumChannels]sv.Quality {
	if st == nil {
		return [sv.NumChannels]sv.Quality{}
	}
	return st.Quality
}

func statsFrequency(st *svsub.StreamStats) [sv.NumChannels]float64 {
	if st == nil {
		return [sv.NumChannels]float64{}
	}
	return st.FrequencyHz
}

func statsSynch(st *svsub.StreamStats) sv.SmpSynch {
	if st == nil {
		return sv.SmpSynchNone
	}
	return st.SmpSynch
}

func statsSimulation(st *svsub.StreamStats) bool {
	if st == nil {
		return false
	}
	return st.Simulation
}
