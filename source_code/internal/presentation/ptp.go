package presentation

import (
	"fmt"
	"image/color"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	pbmtruntime "pbmt/internal/application/control"
	"pbmt/internal/config"
	ptpdomain "pbmt/internal/domain/ptp"
)

func automaticPTPClockIdentity(ifaceName string, clientRole bool) string {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil || len(iface.HardwareAddr) < 6 {
		return "available after start"
	}
	var mac [6]byte
	copy(mac[:], iface.HardwareAddr[:6])
	if clientRole {
		return formatPTPClockIdentity(ptpdomain.ClientClockIdentityFromMAC(mac))
	}
	return formatPTPClockIdentity(ptpdomain.ClockIdentityFromMAC(mac))
}

func automaticPTPPortIdentity(ifaceName string, clientRole bool) string {
	identity := automaticPTPClockIdentity(ifaceName, clientRole)
	if identity == "available after start" {
		return identity
	}
	return fmt.Sprintf("%s / port 1", identity)
}

func formatPTPClockIdentity(identity ptpdomain.ClockIdentity) string {
	return strings.ToUpper(identity.String())
}

func formatPTPPortIdentity(identity ptpdomain.PortIdentity) string {
	return fmt.Sprintf("%s / port %d", formatPTPClockIdentity(identity.ClockIdentity), identity.PortNumber)
}

func formatPTPInterval(logInterval int8) string {
	interval := time.Duration(float64(time.Second) * ptpIntervalSeconds(logInterval))
	return fmt.Sprintf("%s (log=%d)", interval, logInterval)
}

func ptpIntervalSeconds(logInterval int8) float64 {
	return math.Pow(2, float64(logInterval))
}

func applyPTPDelayMechanism(profile *ptpdomain.Profile, mechanism string) {
	if profile.C37238Version != ptpdomain.C37238Disabled {
		profile.DelayMechanism = ptpdomain.DelayMechanismP2P
		return
	}
	switch strings.ToLower(mechanism) {
	case "e2e":
		profile.DelayMechanism = ptpdomain.DelayMechanismE2E
	case "p2p":
		profile.DelayMechanism = ptpdomain.DelayMechanismP2P
	}
}

func (s *uiState) ptpClientRows() [][2]string {
	if s.cfg == nil {
		return [][2]string{{"State", "config not loaded"}}
	}
	c := &s.cfg.PTPClient
	if c.Source == "local" {
		return s.localPTPClientRows()
	}
	profile := ptpdomain.DefaultProfile
	if c.Profile == "power" {
		profile = ptpdomain.PowerProfile
		if configured, ok := ptpdomain.ParseC37238Version(firstNonEmpty(c.PowerProfile.Version, "2011")); ok {
			profile.C37238Version = configured
		}
	}
	if c.DomainNumber != nil {
		profile.DomainNumber = *c.DomainNumber
	}
	applyPTPDelayMechanism(&profile, c.DelayMechanism)
	stateName := s.moduleStatus(modulePTPClient)
	transportName := firstNonEmpty(c.Transport, "udp")
	timestampName := "auto"
	clockSource := "System"
	localPort := automaticPTPPortIdentity(c.Interface, true)
	masterPort := "waiting for Announce"
	grandmaster := "waiting for Announce"
	utcOffset := "unknown"
	twoStep := "unknown"
	offset := "not measured"
	meanPathDelay := "not measured"
	frequency := "0 ppb"
	applicationOffset := "0s"
	servo := "UNLOCKED"
	applicationTime := time.Now().UTC().Format("2006-01-02 15:04:05.000000000")
	lastSync := "never"
	syncCount := uint64(0)
	pdelayRespCount := uint64(0)
	expectedC37238Version := profile.C37238Version
	masterC37238Version := ptpdomain.C37238Disabled
	c37238GrandmasterID := uint16(0)
	grandmasterTimeInaccuracy := uint32(0)
	networkTimeInaccuracy := uint32(0)
	totalTimeInaccuracy := uint32(0)
	var alternateTimeOffset *ptpdomain.AlternateTimeOffsetTLV
	if s.runtime != nil {
		if snapshot, ok := s.runtime.PTPClientSnapshot(); ok {
			stateName = snapshot.State.String()
			profile.Name = snapshot.Profile
			profile.DomainNumber = snapshot.DomainNumber
			profile.C37238Version = snapshot.ExpectedC37238Version
			applyPTPDelayMechanism(&profile, snapshot.DelayMechanism)
			expectedC37238Version = snapshot.ExpectedC37238Version
			masterC37238Version = snapshot.MasterC37238Version
			c37238GrandmasterID = snapshot.C37238GrandmasterID
			grandmasterTimeInaccuracy = snapshot.GrandmasterTimeInaccuracy
			networkTimeInaccuracy = snapshot.NetworkTimeInaccuracy
			totalTimeInaccuracy = snapshot.TotalTimeInaccuracy
			alternateTimeOffset = snapshot.AlternateTimeOffset
			transportName = snapshot.Transport
			timestampName = snapshot.TimestampMode + " (selected automatically)"
			if snapshot.MasterPortIdentity != (ptpdomain.PortIdentity{}) {
				masterPort = formatPTPPortIdentity(snapshot.MasterPortIdentity)
			}
			localPort = formatPTPPortIdentity(snapshot.LocalPortIdentity)
			if snapshot.GrandmasterIdentity != (ptpdomain.ClockIdentity{}) {
				grandmaster = formatPTPClockIdentity(snapshot.GrandmasterIdentity)
			}
			if snapshot.MasterPortIdentity != (ptpdomain.PortIdentity{}) {
				utcOffset = fmt.Sprintf("%d s", snapshot.UTCOffset)
			}
			if snapshot.SyncCount > 0 {
				twoStep = strconv.FormatBool(snapshot.TwoStep)
				offset = snapshot.Offset.String()
				meanPathDelay = snapshot.MeanPathDelay.String()
			}
			frequency = fmt.Sprintf("%.3f ppb", snapshot.FrequencyPPB)
			applicationOffset = snapshot.ClockStatus.Offset.String()
			servo = snapshot.ServoState.String()
			applicationTime = snapshot.ApplicationTime.UTC().Format("2006-01-02 15:04:05.000000000")
			if snapshot.ClockStatus.PTPEnabled {
				if snapshot.ClockStatus.Synchronized {
					clockSource = "PTP (synchronized)"
				} else {
					clockSource = "PTP (not synchronized)"
				}
			}
			if !snapshot.LastSync.IsZero() {
				lastSync = snapshot.LastSync.UTC().Format("2006-01-02 15:04:05.000000000")
			}
			syncCount = snapshot.SyncCount
			pdelayRespCount = snapshot.PDelayRespCount
		}
	}
	powerProfile := expectedC37238Version != ptpdomain.C37238Disabled
	masterPowerProfile := masterC37238Version != ptpdomain.C37238Disabled
	masterC37238TLV := "not required"
	if powerProfile {
		masterC37238TLV = "waiting for compatible Announce"
		if masterPowerProfile {
			masterC37238TLV = formatC37238TLV(masterC37238Version)
		}
	}
	alternateStatus := "not received"
	if alternateTimeOffset != nil {
		alternateStatus = "received (type 0x0009)"
	}
	return [][2]string{
		{"State", stateName},
		{"Source PTP", "External"},
		{"Interface", c.Interface},
		{"PTP profile", profile.Name},
		{"Delay mechanism", ptpdomain.DelayMechanismName(profile.DelayMechanism)},
		{"Transport", strings.ToUpper(transportName)},
		{"Domain number", strconv.Itoa(int(profile.DomainNumber))},
		{"Timestamp", timestampName},
		{"Local port identity", localPort},
		{"Clock source for GOOSE/SV", clockSource},
		{"Application time", applicationTime},
		{"Application clock offset vs system", applicationOffset},
		{"Master port identity", masterPort},
		{"Grandmaster identity", grandmaster},
		{"Current UTC offset", utcOffset},
		{"Expected C37.238 TLV", formatC37238TLV(expectedC37238Version)},
		{"Master C37.238 TLV", masterC37238TLV},
		{"C37.238 Grandmaster ID", valueWhen(masterPowerProfile, fmt.Sprintf("%d (0x%04X)", c37238GrandmasterID, c37238GrandmasterID), "not received")},
		{"Grandmaster time inaccuracy", valueWhen(masterC37238Version == ptpdomain.C37238Version2011, fmt.Sprintf("%d ns", grandmasterTimeInaccuracy), "not received")},
		{"Network time inaccuracy", valueWhen(masterC37238Version == ptpdomain.C37238Version2011, fmt.Sprintf("%d ns", networkTimeInaccuracy), "not received")},
		{"Total time inaccuracy", valueWhen(masterC37238Version == ptpdomain.C37238Version2017, formatC37238Inaccuracy(totalTimeInaccuracy), "not received")},
		{"Alternate time offset TLV", valueWhen(powerProfile, alternateStatus, "not required")},
		{"Alternate key field", valueWhen(alternateTimeOffset != nil, strconv.Itoa(int(pointerValueOrAlternate(alternateTimeOffset).KeyField)), "not received")},
		{"Alternate current offset", valueWhen(alternateTimeOffset != nil, fmt.Sprintf("%d s", pointerValueOrAlternate(alternateTimeOffset).CurrentOffset), "not received")},
		{"Alternate jump seconds", valueWhen(alternateTimeOffset != nil, fmt.Sprintf("%d s", pointerValueOrAlternate(alternateTimeOffset).JumpSeconds), "not received")},
		{"Alternate time of next jump", valueWhen(alternateTimeOffset != nil, strconv.FormatUint(pointerValueOrAlternate(alternateTimeOffset).TimeOfNextJump, 10), "not received")},
		{"Alternate display name", valueWhen(alternateTimeOffset != nil, pointerValueOrAlternate(alternateTimeOffset).DisplayName, "not received")},
		{"Two step", twoStep},
		{"Servo", servo},
		{"Offset", offset},
		{"Mean path delay", meanPathDelay},
		{"Frequency correction", frequency},
		{"Last synchronization", lastSync},
		{"Sync samples", strconv.FormatUint(syncCount, 10)},
		{"Peer-delay response pairs sent", strconv.FormatUint(pdelayRespCount, 10)},
	}
}

func (s *uiState) localPTPClientRows() [][2]string {
	state := s.moduleStatus(modulePTPClient)
	clockSource := "not bound"
	applicationTime := "not available"
	grandmaster := "not bound"
	profile, domain, utcOffset := "—", "—", "—"
	if s.runtime != nil {
		if snapshot, ok := s.runtime.PTPClientSnapshot(); ok && snapshot.Source == "local" {
			state = "SOURCE LOST"
			clockSource = "Local holdover (not synchronized; restart Client to rebind)"
			if snapshot.ClockStatus.Synchronized {
				state = "LOCAL BOUND"
				clockSource = "Local PTP Server (internal binding, not a network measurement)"
			}
			applicationTime = snapshot.ApplicationTime.UTC().Format("2006-01-02 15:04:05.000000000")
			grandmaster = formatPTPClockIdentity(snapshot.GrandmasterIdentity)
			profile = snapshot.Profile
			domain = strconv.Itoa(int(snapshot.DomainNumber))
			utcOffset = fmt.Sprintf("%d s", snapshot.UTCOffset)
		}
	}
	return [][2]string{
		{"State", state}, {"Source PTP", "Local"},
		{"Clock source for GOOSE/SV", clockSource}, {"Application time", applicationTime},
		{"Grandmaster identity", grandmaster}, {"Server PTP profile", profile},
		{"Server domain number", domain}, {"Current UTC offset", utcOffset},
		{"Network exchange", "none"}, {"Automatic source switching", "disabled"},
		{"Start order", "Start PTP Server, then PTP Client"},
	}
}

func pointerValueOrAlternate(value *ptpdomain.AlternateTimeOffsetTLV) ptpdomain.AlternateTimeOffsetTLV {
	if value == nil {
		return ptpdomain.AlternateTimeOffsetTLV{}
	}
	return *value
}

func (s *uiState) ptpServerRows() [][2]string {
	if s.cfg == nil {
		return [][2]string{{"State", "config not loaded"}}
	}
	c := &s.cfg.PTPServer
	profile := ptpdomain.DefaultProfile
	if c.Profile == "power" {
		profile = ptpdomain.PowerProfile
	}
	if c.DomainNumber != nil {
		profile.DomainNumber = *c.DomainNumber
	}
	applyPTPDelayMechanism(&profile, c.DelayMechanism)
	if c.Priority1 != nil {
		profile.Priority1 = *c.Priority1
	}
	if c.Priority2 != nil {
		profile.Priority2 = *c.Priority2
	}
	if c.ClockClass != nil {
		profile.ClockClass = *c.ClockClass
	}
	if c.ClockAccuracy != nil {
		profile.ClockAccuracy = *c.ClockAccuracy
	}
	if c.ClockVariance != nil {
		profile.ClockVariance = *c.ClockVariance
	}
	if source, ok := config.ParseTimeSource(c.TimeSource); ok {
		profile.TimeSource = source
	}
	if c.TimeTraceable != nil {
		if *c.TimeTraceable {
			profile.FlagField |= ptpdomain.FlagTimeTraceable
		} else {
			profile.FlagField &^= ptpdomain.FlagTimeTraceable
		}
	}
	if c.FrequencyTraceable != nil {
		if *c.FrequencyTraceable {
			profile.FlagField |= ptpdomain.FlagFrequencyTraceable
		} else {
			profile.FlagField &^= ptpdomain.FlagFrequencyTraceable
		}
	}
	utcOffset := int16(37)
	if c.UTCOffset != nil {
		utcOffset = *c.UTCOffset
	}
	transportName := firstNonEmpty(c.Transport, "udp")
	timestampName := "hardware"
	identity := automaticPTPClockIdentity(c.Interface, false)
	portIdentity := automaticPTPPortIdentity(c.Interface, false)
	stateName := s.moduleStatus(modulePTPServer)
	clockVariance := profile.ClockVariance
	transportSpecific := profile.TransportSpecific
	logSyncInterval := profile.LogSyncInterval
	logAnnounceInterval := profile.LogAnnounceInterval
	c37Version := profile.C37238Version
	if configured, ok := ptpdomain.ParseC37238Version(c.PowerProfile.Version); ok && c.Profile == "power" {
		c37Version = configured
	}
	grandmasterIDValue := pointerValueOr(c.PowerProfile.GrandmasterID, uint16(3))
	grandmasterTimeInaccuracy := pointerValueOr(c.PowerProfile.GrandmasterTimeInaccuracy, uint32(60))
	networkTimeInaccuracy := pointerValueOr(c.PowerProfile.NetworkTimeInaccuracy, uint32(0))
	totalTimeInaccuracy := pointerValueOr(c.PowerProfile.TotalTimeInaccuracy, uint32(100))
	alternateEnabled := c37Version != ptpdomain.C37238Disabled && pointerValueOr(c.PowerProfile.AlternateTimeOffset.Enabled, true)
	alternateKeyField := pointerValueOr(c.PowerProfile.AlternateTimeOffset.KeyField, uint8(1))
	alternateCurrentOffset := pointerValueOr(c.PowerProfile.AlternateTimeOffset.CurrentOffset, int32(10763))
	alternateJumpSeconds := pointerValueOr(c.PowerProfile.AlternateTimeOffset.JumpSeconds, int32(0))
	alternateTimeOfNextJump := pointerValueOr(c.PowerProfile.AlternateTimeOffset.TimeOfNextJump, uint64(0))
	alternateDisplayName := firstNonEmpty(c.PowerProfile.AlternateTimeOffset.DisplayName, "UTC+03:00")
	announceCount := uint64(0)
	syncCount := uint64(0)
	delayRespCount := uint64(0)
	pdelayRespCount := uint64(0)
	if s.runtime != nil {
		if snapshot, ok := s.runtime.PTPServerSnapshot(); ok {
			stateName = snapshot.State.String()
			profile.Name = snapshot.Profile
			applyPTPDelayMechanism(&profile, snapshot.DelayMechanism)
			profile.DomainNumber = snapshot.DomainNumber
			profile.Priority1 = snapshot.Priority1
			profile.Priority2 = snapshot.Priority2
			profile.ClockClass = snapshot.GrandmasterClockClass
			profile.ClockAccuracy = snapshot.GrandmasterClockAccuracy
			clockVariance = snapshot.GrandmasterClockVariance
			profile.TimeSource = snapshot.TimeSource
			transportSpecific = snapshot.TransportSpecific
			logSyncInterval = snapshot.LogSyncInterval
			logAnnounceInterval = snapshot.LogAnnounceInterval
			c37Version = snapshot.C37238Version
			if snapshot.UTCOffsetValid {
				profile.FlagField |= ptpdomain.FlagCurrentUtcOffsetValid
			} else {
				profile.FlagField &^= ptpdomain.FlagCurrentUtcOffsetValid
			}
			if snapshot.TimeTraceable {
				profile.FlagField |= ptpdomain.FlagTimeTraceable
			} else {
				profile.FlagField &^= ptpdomain.FlagTimeTraceable
			}
			if snapshot.FrequencyTraceable {
				profile.FlagField |= ptpdomain.FlagFrequencyTraceable
			} else {
				profile.FlagField &^= ptpdomain.FlagFrequencyTraceable
			}
			utcOffset = snapshot.CurrentUTCOffset
			transportName = snapshot.Transport
			identity = formatPTPClockIdentity(snapshot.GrandmasterClockIdentity)
			portIdentity = formatPTPPortIdentity(ptpdomain.PortIdentity{
				ClockIdentity: snapshot.GrandmasterClockIdentity,
				PortNumber:    snapshot.PortNumber,
			})
			grandmasterIDValue = snapshot.C37238GrandmasterID
			grandmasterTimeInaccuracy = snapshot.GrandmasterTimeInaccuracy
			networkTimeInaccuracy = snapshot.NetworkTimeInaccuracy
			totalTimeInaccuracy = snapshot.TotalTimeInaccuracy
			alternateEnabled = snapshot.AlternateTimeOffset != nil
			if snapshot.AlternateTimeOffset != nil {
				alternateKeyField = snapshot.AlternateTimeOffset.KeyField
				alternateCurrentOffset = snapshot.AlternateTimeOffset.CurrentOffset
				alternateJumpSeconds = snapshot.AlternateTimeOffset.JumpSeconds
				alternateTimeOfNextJump = snapshot.AlternateTimeOffset.TimeOfNextJump
				alternateDisplayName = snapshot.AlternateTimeOffset.DisplayName
			}
			announceCount = snapshot.AnnounceCount
			syncCount = snapshot.SyncCount
			delayRespCount = snapshot.DelayRespCount
			pdelayRespCount = snapshot.PDelayRespCount
		}
	}
	return [][2]string{
		{"State", stateName},
		{"Interface", c.Interface},
		{"PTP profile", profile.Name},
		{"Delay mechanism", ptpdomain.DelayMechanismName(profile.DelayMechanism)},
		{"Two step", "true"},
		{"Transport", strings.ToUpper(transportName)},
		{"PTP version", "2"},
		{"transportSpecific", fmt.Sprintf("%d (0x%X)", transportSpecific, transportSpecific)},
		{"Domain number", strconv.Itoa(int(profile.DomainNumber))},
		{"Sync interval", formatPTPInterval(logSyncInterval)},
		{"Announce interval", formatPTPInterval(logAnnounceInterval)},
		{"Current UTC offset", fmt.Sprintf("%d s", utcOffset)},
		{"UTC offset valid", strconv.FormatBool(profile.FlagField&ptpdomain.FlagCurrentUtcOffsetValid != 0)},
		{"Priority 1", strconv.Itoa(int(profile.Priority1))},
		{"Priority 2", strconv.Itoa(int(profile.Priority2))},
		{"Grandmaster clock class", fmt.Sprintf("%s (%d / 0x%02X)", ptpdomain.ClockClassName(profile.ClockClass), profile.ClockClass, profile.ClockClass)},
		{"Grandmaster clock accuracy", fmt.Sprintf("%s (0x%02X)", ptpdomain.ClockAccuracyName(profile.ClockAccuracy), profile.ClockAccuracy)},
		{"Offset scaled log variance", fmt.Sprintf("%s (0x%04X)", ptpdomain.ClockVarianceName(clockVariance), clockVariance)},
		{"Grandmaster clock identity", identity},
		{"Grandmaster port identity", portIdentity},
		{"Time source", fmt.Sprintf("%s (0x%02X)", ptpdomain.TimeSourceName(profile.TimeSource), profile.TimeSource)},
		{"Time traceable", strconv.FormatBool(profile.FlagField&ptpdomain.FlagTimeTraceable != 0)},
		{"Frequency traceable", strconv.FormatBool(profile.FlagField&ptpdomain.FlagFrequencyTraceable != 0)},
		{"C37.238 TLV", formatC37238TLV(c37Version)},
		{"C37.238 Grandmaster ID", valueWhen(c37Version != ptpdomain.C37238Disabled, fmt.Sprintf("%d (0x%04X)", grandmasterIDValue, grandmasterIDValue), "not transmitted")},
		{"Grandmaster time inaccuracy", valueWhen(c37Version == ptpdomain.C37238Version2011, fmt.Sprintf("%d ns", grandmasterTimeInaccuracy), "not transmitted")},
		{"Network time inaccuracy", valueWhen(c37Version == ptpdomain.C37238Version2011, fmt.Sprintf("%d ns", networkTimeInaccuracy), "not transmitted")},
		{"Total time inaccuracy", valueWhen(c37Version == ptpdomain.C37238Version2017, formatC37238Inaccuracy(totalTimeInaccuracy), "not transmitted")},
		{"Alternate time offset TLV", valueWhen(alternateEnabled, "enabled (type 0x0009)", "not transmitted")},
		{"Alternate key field", valueWhen(alternateEnabled, strconv.Itoa(int(alternateKeyField)), "not transmitted")},
		{"Alternate current offset", valueWhen(alternateEnabled, fmt.Sprintf("%d s", alternateCurrentOffset), "not transmitted")},
		{"Alternate jump seconds", valueWhen(alternateEnabled, fmt.Sprintf("%d s", alternateJumpSeconds), "not transmitted")},
		{"Alternate time of next jump", valueWhen(alternateEnabled, strconv.FormatUint(alternateTimeOfNextJump, 10), "not transmitted")},
		{"Alternate display name", valueWhen(alternateEnabled, alternateDisplayName, "not transmitted")},
		{"Timestamp", timestampName},
		{"Announce sent", strconv.FormatUint(announceCount, 10)},
		{"Sync sent", strconv.FormatUint(syncCount, 10)},
		{"Delay responses sent", strconv.FormatUint(delayRespCount, 10)},
		{"Peer-delay response pairs sent", strconv.FormatUint(pdelayRespCount, 10)},
	}
}

func formatC37238TLV(version ptpdomain.C37238Version) string {
	if version == ptpdomain.C37238Disabled {
		return "disabled"
	}
	subtype := 1
	if version == ptpdomain.C37238Version2017 {
		subtype = 2
	}
	return fmt.Sprintf("enabled (version %s, OUI 1C:12:9D, subtype 0x%06X)", version, subtype)
}

func formatC37238Inaccuracy(value uint32) string {
	if value == ^uint32(0) {
		return "Unknown (0xFFFFFFFF)"
	}
	return fmt.Sprintf("%d ns", value)
}

func pointerValueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

func valueWhen(condition bool, ifTrue, ifFalse string) string {
	if condition {
		return ifTrue
	}
	return ifFalse
}

func (s *uiState) ptpServerControl() (fyne.CanvasObject, func()) {
	return s.ptpStatusControl(
		"PTP Server / Forced Grandmaster",
		"Forced Grandmaster mode for protection and automation device testing.",
		"Grandmaster Parameters",
		pbmtruntime.ModulePTPServer,
		s.ptpServerRows,
	)
}

func (s *uiState) ptpClientControl() (fyne.CanvasObject, func()) {
	return s.ptpStatusControl(
		"PTP Client / Application Clock",
		"PTP disciplines only the application's internal clock; system time is not changed.",
		"Client and Application Clock",
		pbmtruntime.ModulePTPClient,
		s.ptpClientRows,
	)
}

func (s *uiState) ptpStatusControl(title, descriptionText, cardTitle string, runtimeID pbmtruntime.ModuleID, rowProvider func() [][2]string) (fyne.CanvasObject, func()) {
	header := canvas.NewText(title, color.White)
	header.TextSize = 22
	header.TextStyle = fyne.TextStyle{Bold: true}
	description := widget.NewLabel(descriptionText)
	description.Wrapping = fyne.TextWrapWord

	form := widget.NewForm()
	labels := make(map[string]*widget.Label)
	refresh := func() {
		rows := rowProvider()
		if len(labels) == 0 {
			for _, row := range rows {
				label := widget.NewLabel(row[1])
				label.Wrapping = fyne.TextWrapWord
				labels[row[0]] = label
				form.Append(row[0], label)
			}
			return
		}
		for _, row := range rows {
			if label := labels[row[0]]; label != nil {
				setLabelTextIfChanged(label, row[1])
			}
		}
	}
	refresh()

	events := widget.NewMultiLineEntry()
	events.SetText("No runtime data yet.")
	events.Disable()
	done := make(chan struct{})
	var cancelOnce sync.Once
	stopEvents := func() {}
	if s.runtime != nil {
		ch, stop := s.runtime.Subscribe(128)
		stopEvents = stop
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			var lines []string
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					fyne.Do(refresh)
				case event, ok := <-ch:
					if !ok {
						return
					}
					if event.Module != runtimeID {
						continue
					}
					lines = append(lines, fmt.Sprintf("%s [%s] %s", event.Time.Format("15:04:05.000"), event.Severity, event.Message))
					if len(lines) > 300 {
						lines = lines[len(lines)-300:]
					}
					value := strings.Join(lines, "\n")
					fyne.Do(func() { events.SetText(value) })
				}
			}
		}()
	}
	cancel := func() {
		cancelOnce.Do(func() {
			close(done)
			stopEvents()
		})
	}
	content := container.NewBorder(
		container.NewVBox(header, description),
		nil, nil, nil,
		container.NewVScroll(container.NewVBox(
			widget.NewCard(cardTitle, "", form),
			widget.NewCard("Runtime Events", "", events),
		)),
	)
	return container.NewPadded(content), cancel
}
