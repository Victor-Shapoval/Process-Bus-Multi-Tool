package presentation

import (
	"fmt"
	"image/color"
	"math"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	pbmtruntime "pbmt/internal/application/control"
	"pbmt/internal/domain/sv"
)

func (s *uiState) svSubscriberControl() (fyne.CanvasObject, func()) {
	header := canvas.NewText("SV Monitor", color.White)
	header.TextSize = 22
	header.TextStyle = fyne.TextStyle{Bold: true}
	body := widget.NewLabel("Incoming sampled values, channel quality and smpCnt diagnostics will be shown here.")
	body.Wrapping = fyne.TextWrapWord

	channels := sv.ChannelNames[:]

	type svSubscriberPanel struct {
		labels         [][]*widget.Label
		valueHeader    *widget.Label
		synchInfo      *widget.Label
		simulationInfo *widget.Label
		eventInfo      *widget.Label
		showInstant    bool
		lastValues     [][]string
	}
	type svSubscriberDisplayConfig struct {
		currentCoefficient float64
		voltageCoefficient float64
		baseVectorIndex    int
	}

	panels := make(map[string]*svSubscriberPanel)
	displayConfigs := make(map[string]svSubscriberDisplayConfig)
	channelChecks := container.NewGridWithColumns(len(channels))
	for i, ch := range channels {
		idx := i
		check := widget.NewCheck(ch, func(v bool) {
			for _, panel := range panels {
				if idx >= len(panel.labels) {
					continue
				}
				if v {
					for _, label := range panel.labels[idx] {
						label.Show()
					}
				} else {
					for _, label := range panel.labels[idx] {
						label.Hide()
					}
				}
			}
		})
		check.SetChecked(true)
		channelChecks.Add(check)
	}

	streamPanels := container.NewVBox()
	addPanel := func(name string) {
		valueHeader := widget.NewLabel("RMS")
		panel := &svSubscriberPanel{
			labels:         make([][]*widget.Label, len(channels)),
			valueHeader:    valueHeader,
			synchInfo:      widget.NewLabel("Sync: -"),
			simulationInfo: widget.NewLabel("Simulation/Test: false"),
			eventInfo:      widget.NewLabel("Last event: no data"),
			lastValues:     make([][]string, len(channels)),
		}
		panel.eventInfo.Wrapping = fyne.TextWrapWord
		instantCheck := widget.NewCheck("Instant", func(v bool) {
			panel.showInstant = v
			if panel.showInstant {
				valueHeader.SetText("Instant")
			} else {
				valueHeader.SetText("RMS")
			}
		})
		counters := container.NewGridWithColumns(5,
			widget.NewLabel("Channel"),
			widget.NewLabel("Quality"),
			valueHeader,
			widget.NewLabel("Angle"),
			widget.NewLabel("Frequency"),
		)
		for i, ch := range channels {
			row := []string{ch, "No data", "-", "-", "-"}
			labels := make([]*widget.Label, len(row))
			for j, text := range row {
				labels[j] = widget.NewLabel(text)
				counters.Add(labels[j])
			}
			panel.labels[i] = labels
			panel.lastValues[i] = append([]string(nil), row...)
		}

		title := canvas.NewText(name, color.White)
		title.TextStyle = fyne.TextStyle{Bold: true}
		title.TextSize = 20
		panelHeader := container.NewHBox(title, layout.NewSpacer(), panel.simulationInfo, panel.synchInfo, instantCheck)
		panels[name] = panel
		streamPanels.Add(widget.NewCard("", "", container.NewVBox(panelHeader, panel.eventInfo, counters)))
	}

	if s.cfg != nil && len(s.cfg.SVSub.Subscriptions) > 0 {
		for _, sub := range s.cfg.SVSub.Subscriptions {
			name := sub.Name
			if strings.TrimSpace(name) == "" {
				name = sub.SvID
			}
			if strings.TrimSpace(name) == "" {
				name = "SV Subscriber"
			}
			addPanel(name)
			ki := sub.CurrentCoefficient
			if ki == 0 {
				ki = 1
			}
			ku := sub.VoltageCoefficient
			if ku == 0 {
				ku = 1
			}
			displayConfigs[name] = svSubscriberDisplayConfig{
				currentCoefficient: ki,
				voltageCoefficient: ku,
				baseVectorIndex:    svChannelIndex(sub.BaseVector),
			}
		}
	} else {
		addPanel("SV Subscriber")
		displayConfigs["SV Subscriber"] = svSubscriberDisplayConfig{
			currentCoefficient: 1,
			voltageCoefficient: 1,
			baseVectorIndex:    sv.ChUa,
		}
	}

	content := container.NewBorder(
		widget.NewCard("Channels", "", channelChecks),
		nil,
		nil,
		nil,
		container.NewVScroll(streamPanels),
	)

	cancel := func() {}
	if s.runtime != nil {
		ch, stop := s.runtime.Subscribe(128)
		cancel = stop
		go func() {
			for event := range ch {
				if event.Module != pbmtruntime.ModuleSVSub {
					continue
				}
				panel := panels[event.Subscription]
				if panel == nil {
					continue
				}
				eventText := fmt.Sprintf("Last event: %s [%s] %s", event.Time.Format("15:04:05.000"), event.Severity, event.Message)
				fyne.Do(func() { setLabelTextIfChanged(panel.eventInfo, eventText) })
				if event.HasSVValues {
					displayConfig := displayConfigs[event.Subscription]
					ki := displayConfig.currentCoefficient
					if ki == 0 {
						ki = 1
					}
					ku := displayConfig.voltageCoefficient
					if ku == 0 {
						ku = 1
					}
					values := event.SVValues
					rms := event.SVRMS
					angles := relativeSVAngles(event.SVAngle, displayConfig.baseVectorIndex)
					quality := event.SVQuality
					for i := range values {
						if i >= sv.ChUa {
							values[i] *= ku
							rms[i] *= ku
						} else {
							values[i] *= ki
							rms[i] *= ki
						}
					}
					fyne.Do(func() {
						synchText := event.SVSynch.String()
						setLabelTextIfChanged(panel.synchInfo, "Sync: "+synchText)
						setLabelTextIfChanged(panel.simulationInfo, fmt.Sprintf("Simulation/Test: %t", event.SVSimulation))
						for i := range panel.labels {
							displayValue := rms[i]
							if panel.showInstant {
								displayValue = values[i]
							}
							row := []string{
								sv.ChannelNames[i],
								qualityText(quality[i]),
								fmt.Sprintf("%.3f", displayValue),
								formatAngle(angles[i]),
								formatFrequency(event.SVFrequency[i]),
							}
							for j, text := range row {
								if panel.lastValues[i][j] == text {
									continue
								}
								panel.lastValues[i][j] = text
								panel.labels[i][j].SetText(text)
							}
						}
					})
				}
			}
		}()
	}

	return container.NewPadded(container.NewBorder(container.NewVBox(header, body), nil, nil, nil, content)), cancel
}

func setLabelTextIfChanged(label *widget.Label, text string) {
	if label.Text == text {
		return
	}
	label.SetText(text)
}

func svChannelIndex(name string) int {
	for i, ch := range sv.ChannelNames {
		if ch == name {
			return i
		}
	}
	return sv.ChUa
}

func relativeSVAngles(angles [sv.NumChannels]float64, baseIndex int) [sv.NumChannels]float64 {
	var out [sv.NumChannels]float64
	base := 0.0
	if baseIndex >= 0 && baseIndex < sv.NumChannels {
		base = angles[baseIndex]
	}
	for ch := 0; ch < sv.NumChannels; ch++ {
		out[ch] = normalizeDegrees(angles[ch] - base)
	}
	return out
}

func formatAngle(v float64) string {
	if math.IsNaN(v) {
		return "unstable"
	}
	return fmt.Sprintf("%.2f°", v)
}

func normalizeDegrees(v float64) float64 {
	for v > 180 {
		v -= 360
	}
	for v <= -180 {
		v += 360
	}
	return v
}

func qualityText(q sv.Quality) string {
	if q == sv.QualityValid {
		return "Good"
	}
	flags := q.Flags()
	if len(flags) == 0 {
		return fmt.Sprintf("0x%08X", uint32(q))
	}
	return strings.Join(flags, ",")
}

func formatFrequency(v float64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f Hz", v)
}
