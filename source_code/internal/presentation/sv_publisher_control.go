package presentation

import (
	"fmt"
	"image/color"
	"log/slog"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"pbmt/internal/application/svpub"
	"pbmt/internal/domain/sv"
)

func (s *uiState) svPublisherControl(owner fyne.Window) (fyne.CanvasObject, func(), func()) {
	header := canvas.NewText("SV Publisher Control", color.White)
	header.TextSize = 22
	header.TextStyle = fyne.TextStyle{Bold: true}

	rows := container.NewVBox()
	var streamAppliers []func()
	var streamDisposers []func()
	var timingRefreshers []func()
	done := make(chan struct{})
	var closeOnce sync.Once
	if s.cfg != nil {
		for _, stream := range s.cfg.SVPub.Streams {
			stream := stream
			manualState := s.svPublisherManualState(stream)
			baseFreq := widget.NewEntry()
			baseFreq.SetText(manualState.BaseFrequency)
			baseFreq.SetPlaceHolder("50.00")
			simulation := widget.NewCheck("Simulation/Test", nil)
			simulation.SetChecked(manualState.Simulation)
			calculateNeutral := widget.NewCheck("Calculate Un/In", nil)
			calculateNeutral.SetChecked(manualState.CalculateNeutral)
			syncSelect := widget.NewSelect(svPublisherSynchOptions(), nil)
			syncSelect.SetSelected(manualState.Sync)

			var allRows []svPublisherChannelRow
			var currentControls *svPublisherModeControls
			var voltageControls *svPublisherModeControls
			useBaseFreq := widget.NewCheck("Use for all channels", func(v bool) {
				if v {
					setSVPublisherRowsFrequency(allRows, baseFreq.Text)
				}
				refreshSVPublisherModeControls(currentControls)
				refreshSVPublisherModeControls(voltageControls)
			})
			useBaseFreq.SetChecked(true)
			baseFreq.OnChanged = func(text string) {
				if useBaseFreq.Checked {
					setSVPublisherRowsFrequency(allRows, text)
				}
				refreshSVPublisherModeControls(currentControls)
				refreshSVPublisherModeControls(voltageControls)
			}

			currentRows, currentGrid := newSVPublisherChannelGrid([]int{sv.ChIa, sv.ChIb, sv.ChIc, sv.ChIn}, stream.SampleTimingFrequency, owner)
			voltageRows, voltageGrid := newSVPublisherChannelGrid([]int{sv.ChUa, sv.ChUb, sv.ChUc, sv.ChUn}, stream.SampleTimingFrequency, owner)
			allRows = append(allRows, currentRows...)
			allRows = append(allRows, voltageRows...)

			currentControls = newSVPublisherModeControls("I", currentRows, baseFreq, useBaseFreq, stream.SampleTimingFrequency)
			voltageControls = newSVPublisherModeControls("U", voltageRows, baseFreq, useBaseFreq, stream.SampleTimingFrequency)
			applySVPublisherGroupState(currentControls, manualState.Current)
			applySVPublisherGroupState(voltageControls, manualState.Voltage)
			useBaseFreq.SetChecked(manualState.UseBaseFrequency)
			var applyTimerMu sync.Mutex
			var applyTimer *time.Timer
			disposed := false
			isDisposed := func() bool {
				applyTimerMu.Lock()
				defer applyTimerMu.Unlock()
				return disposed
			}
			saveState := func() {
				captureSVPublisherManualState(manualState, baseFreq, simulation, calculateNeutral, syncSelect, useBaseFreq, currentControls, voltageControls)
			}
			lastInvalidField := ""
			reportInvalid := func(field string) {
				if lastInvalidField != field {
					slog.Warn("SV publisher input rejected", "stream", stream.Name, "field", field)
					lastInvalidField = field
				}
			}
			autoApply := func() {
				if isDisposed() {
					return
				}
				if s.runtime == nil || s.moduleStatus(moduleSVPub) != "Running" {
					return
				}
				baseFrequency, err := parseFloatEntry(baseFreq, float64(stream.SampleTimingFrequency))
				if err != nil {
					reportInvalid("base_frequency")
					s.statusBar.SetText("SV Publisher: base frequency: " + err.Error())
					return
				}
				var settings [sv.NumChannels]svpub.ChannelSetting
				if err := applySVPublisherRowsToSettings(&settings, currentControls, baseFrequency, useBaseFreq.Checked, stream.SampleTimingFrequency); err != nil {
					reportInvalid("currents")
					s.statusBar.SetText("SV Publisher currents: " + err.Error())
					return
				}
				if err := applySVPublisherRowsToSettings(&settings, voltageControls, baseFrequency, useBaseFreq.Checked, stream.SampleTimingFrequency); err != nil {
					reportInvalid("voltages")
					s.statusBar.SetText("SV Publisher voltages: " + err.Error())
					return
				}
				if calculateNeutral.Checked {
					applySVPublisherCalculatedNeutral(&settings)
				}
				if err := s.runtime.SetSVPublisherWaveform(stream.Name, settings, simulation.Checked, svPublisherSynchValue(syncSelect.Selected)); err != nil {
					s.statusBar.SetText("SV Publisher: " + err.Error())
					return
				}
				s.statusBar.SetText("SV waveform applied: " + stream.Name)
				lastInvalidField = ""
			}
			scheduleApply := func() {
				saveState()
				applyTimerMu.Lock()
				defer applyTimerMu.Unlock()
				if disposed {
					return
				}
				if applyTimer != nil {
					applyTimer.Stop()
				}
				applyTimer = time.AfterFunc(350*time.Millisecond, func() {
					fyne.Do(autoApply)
				})
			}
			wireSVPublisherAutoApply(scheduleApply, baseFreq, simulation, calculateNeutral, syncSelect, useBaseFreq, currentControls, voltageControls)
			streamAppliers = append(streamAppliers, autoApply)
			streamDisposers = append(streamDisposers, func() {
				applyTimerMu.Lock()
				disposed = true
				if applyTimer != nil {
					applyTimer.Stop()
					applyTimer = nil
				}
				applyTimerMu.Unlock()
			})

			common := container.NewHBox(
				widget.NewLabel("Frequency"),
				baseFreq,
				widget.NewLabel("Hz"),
				useBaseFreq,
				calculateNeutral,
				widget.NewLabel("Sync"),
				syncSelect,
			)
			currents := container.NewVBox(
				widget.NewLabelWithStyle("Currents", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				currentControls.bar,
				currentControls.compPanel,
				currentGrid,
			)
			voltages := container.NewVBox(
				widget.NewLabelWithStyle("Voltages", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				voltageControls.bar,
				voltageControls.compPanel,
				voltageGrid,
			)

			cardHeader := container.NewHBox(
				widget.NewLabel(fmt.Sprintf("svID: %s    APPID: 0x%04X    %d samples/period", stream.SvID, uint16(stream.AppID), stream.SmpRate)),
				simulation,
			)
			timingLabel := widget.NewLabel("TX timing: no runtime data")
			timingLabel.Wrapping = fyne.TextWrapWord
			timingRefreshers = append(timingRefreshers, func() {
				if s.runtime == nil {
					return
				}
				snapshot, ok := s.runtime.SVPublisherSnapshot(stream.Name)
				if !ok || !snapshot.Ready {
					setLabelTextIfChanged(timingLabel, "TX timing: no runtime data")
					return
				}
				stats := snapshot.Timing
				setLabelTextIfChanged(timingLabel, fmt.Sprintf("TX timing: %.0f / %d frames/s | Sent: %d | Skipped: %d | Max lateness: %s (host scheduling)",
					stats.FramesPerSecond, int(stream.SmpRate)*int(stream.SampleTimingFrequency), stats.FramesSent, stats.SkippedSamples, stats.MaxLateness.Round(time.Microsecond)))
			})
			rows.Add(widget.NewCard(stream.Name, "", container.NewVBox(cardHeader, timingLabel, common, widget.NewSeparator(), currents, widget.NewSeparator(), voltages)))
		}
	}
	if len(rows.Objects) == 0 {
		rows.Add(widget.NewLabel("No SV publishers configured."))
	}
	applyNow := func() {
		for _, apply := range streamAppliers {
			apply()
		}
	}

	content := container.NewPadded(container.NewBorder(
		container.NewVBox(header),
		nil,
		nil,
		nil,
		container.NewVScroll(rows),
	))
	if len(timingRefreshers) > 0 && s.runtime != nil {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					fyne.Do(func() {
						select {
						case <-done:
							return
						default:
						}
						for _, refresh := range timingRefreshers {
							refresh()
						}
					})
				}
			}
		}()
	}
	return content, func() {
		closeOnce.Do(func() { close(done) })
		for _, dispose := range streamDisposers {
			dispose()
		}
	}, applyNow
}
