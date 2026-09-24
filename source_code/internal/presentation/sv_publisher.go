package presentation

import (
	"fmt"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/signal"
	"pbmt/internal/domain/sv"
)

type svPublisherChannelRow struct {
	channel int
	label   *widget.Label
	rms     *widget.Entry
	phase   *widget.Entry
	freq    *widget.Entry
	quality *widget.Entry
}

type svPublisherModeControls struct {
	prefix      string
	rows        []svPublisherChannelRow
	mode        *widget.Select
	bar         *fyne.Container
	baseFreq    *widget.Entry
	useBaseFreq *widget.Check
	nominalFreq uint16
	balancedRMS *widget.Entry
	balancedBox *fyne.Container
	twoPhaseRMS *widget.Entry
	twoPhaseBox *fyne.Container
	compRMS     [3]*widget.Entry
	compPhase   [3]*widget.Entry
	compBox     *fyne.Container
	compPanel   *fyne.Container
}

func newSVPublisherChannelGrid(channels []int, nominalFreq uint16, window fyne.Window) ([]svPublisherChannelRow, fyne.CanvasObject) {
	rows := make([]svPublisherChannelRow, 0, len(channels))
	grid := container.NewVBox(
		container.NewHBox(
			svPublisherLabelCell(widget.NewLabelWithStyle("Channel", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			svPublisherInputCell(widget.NewLabelWithStyle("RMS", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			svPublisherInputCell(widget.NewLabelWithStyle("Phase, deg", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			svPublisherInputCell(widget.NewLabelWithStyle("Frequency, Hz", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			svPublisherQualityCell(widget.NewLabelWithStyle("Quality(hex)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		),
	)
	for _, ch := range channels {
		label := widget.NewLabel(sv.ChannelNames[ch])
		rms := widget.NewEntry()
		rms.SetText("0")
		rms.SetPlaceHolder("0.000")
		phase := widget.NewEntry()
		phase.SetText(fmt.Sprintf("%.2f", defaultSVPublisherPhase(ch)))
		freq := widget.NewEntry()
		freq.SetText(fmt.Sprintf("%d", nominalFreq))
		quality := widget.NewEntry()
		quality.SetText(formatSVPublisherQuality(sv.QualityValid))
		quality.SetPlaceHolder("0x00000000")
		qualityButton := widget.NewButton("...", func() {
			showSVPublisherQualityDialog(quality, window)
		})

		row := svPublisherChannelRow{
			channel: ch,
			label:   label,
			rms:     rms,
			phase:   phase,
			freq:    freq,
			quality: quality,
		}
		rows = append(rows, row)
		grid.Add(container.NewHBox(
			svPublisherLabelCell(label),
			svPublisherInputCell(rms),
			svPublisherInputCell(phase),
			svPublisherInputCell(freq),
			svPublisherQualityCell(container.NewBorder(nil, nil, nil, qualityButton, quality)),
		))
	}
	return rows, grid
}

func svPublisherLabelCell(obj fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(72, obj.MinSize().Height), obj)
}

func svPublisherInputCell(obj fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(110, obj.MinSize().Height), obj)
}

func svPublisherQualityCell(obj fyne.CanvasObject) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(190, obj.MinSize().Height), obj)
}

func newSVPublisherModeControls(prefix string, rows []svPublisherChannelRow, baseFreq *widget.Entry, useBaseFreq *widget.Check, nominalFreq uint16) *svPublisherModeControls {
	c := &svPublisherModeControls{
		prefix:      prefix,
		rows:        rows,
		baseFreq:    baseFreq,
		useBaseFreq: useBaseFreq,
		nominalFreq: nominalFreq,
	}
	c.balancedRMS = widget.NewEntry()
	c.balancedRMS.SetText(firstNonZeroSVPublisherRMS(rows))
	c.balancedRMS.SetPlaceHolder("RMS")
	c.balancedRMS.OnChanged = func(string) {
		refreshSVPublisherModeControls(c)
	}
	c.balancedBox = container.NewHBox(widget.NewLabel("ABC RMS"), svPublisherWideEntry(c.balancedRMS))

	c.twoPhaseRMS = widget.NewEntry()
	c.twoPhaseRMS.SetText(firstNonZeroSVPublisherRMS(rows))
	c.twoPhaseRMS.SetPlaceHolder("RMS")
	c.twoPhaseRMS.OnChanged = func(string) {
		refreshSVPublisherModeControls(c)
	}
	c.twoPhaseBox = container.NewHBox(widget.NewLabel("RMS"), svPublisherWideEntry(c.twoPhaseRMS))

	componentNames := []string{"1", "2", "0"}
	componentRows := container.NewVBox(
		container.NewHBox(
			svPublisherLabelCell(widget.NewLabel("")),
			svPublisherInputCell(widget.NewLabelWithStyle("RMS", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
			svPublisherInputCell(widget.NewLabelWithStyle("Phase, deg", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		),
	)
	for i, name := range componentNames {
		c.compRMS[i] = widget.NewEntry()
		c.compRMS[i].SetText("0")
		c.compRMS[i].SetPlaceHolder("RMS")
		c.compPhase[i] = widget.NewEntry()
		c.compPhase[i].SetText("0.00")
		c.compPhase[i].SetPlaceHolder("deg")
		c.compRMS[i].OnChanged = func(string) {
			refreshSVPublisherModeControls(c)
		}
		c.compPhase[i].OnChanged = func(string) {
			refreshSVPublisherModeControls(c)
		}
		componentRows.Add(container.NewHBox(
			svPublisherLabelCell(widget.NewLabel(prefix+name)),
			svPublisherInputCell(c.compRMS[i]),
			svPublisherInputCell(c.compPhase[i]),
		))
	}
	c.compBox = container.NewHBox()
	c.compPanel = componentRows

	c.mode = widget.NewSelect(svPublisherModeOptions(), func(string) {
		refreshSVPublisherModeControls(c)
	})
	c.mode.SetSelected("Manual")
	c.bar = container.NewHBox(widget.NewLabel("Mode"), c.mode, c.balancedBox, c.twoPhaseBox, c.compBox)
	refreshSVPublisherModeControls(c)
	return c
}

func svPublisherWideEntry(entry *widget.Entry) fyne.CanvasObject {
	return container.NewGridWrap(fyne.NewSize(135, entry.MinSize().Height), entry)
}

func (s *uiState) svPublisherManualState(stream config.SVPublisher) *svPublisherManualState {
	if s.svPublisherStates == nil {
		s.svPublisherStates = make(map[string]*svPublisherManualState)
	}
	if st, ok := s.svPublisherStates[stream.Name]; ok {
		return st
	}
	baseFrequency := fmt.Sprintf("%d", stream.SampleTimingFrequency)
	st := &svPublisherManualState{
		BaseFrequency:    baseFrequency,
		UseBaseFrequency: true,
		Sync:             svPublisherSynchOption(sv.SmpSynch(stream.SmpSynch)),
		Current:          newSVPublisherGroupState([]int{sv.ChIa, sv.ChIb, sv.ChIc, sv.ChIn}, baseFrequency),
		Voltage:          newSVPublisherGroupState([]int{sv.ChUa, sv.ChUb, sv.ChUc, sv.ChUn}, baseFrequency),
	}
	s.svPublisherStates[stream.Name] = st
	return st
}

// Returning from MCP starts Manual from the applied waveform.
// Individual-channel mode avoids recomputing phases or the neutral on handover.
func loadSVPublisherSnapshot(st *svPublisherManualState, snapshot svpub.Snapshot) {
	format := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	st.BaseFrequency = format(snapshot.Settings[0].Frequency)
	st.UseBaseFrequency = false
	st.CalculateNeutral = false
	st.Simulation = snapshot.Simulation
	st.Sync = svPublisherSynchOption(snapshot.Synch)
	st.Current = newSVPublisherGroupState([]int{sv.ChIa, sv.ChIb, sv.ChIc, sv.ChIn}, st.BaseFrequency)
	st.Voltage = newSVPublisherGroupState([]int{sv.ChUa, sv.ChUb, sv.ChUc, sv.ChUn}, st.BaseFrequency)
	for ch, value := range snapshot.Settings {
		row := svPublisherRowState{RMS: format(value.RMS), Phase: format(value.PhaseDeg), Frequency: format(value.Frequency), Quality: formatSVPublisherQuality(value.Quality)}
		if ch <= sv.ChIn {
			st.Current.Rows[ch] = row
		} else {
			st.Voltage.Rows[ch] = row
		}
	}
}

func newSVPublisherGroupState(channels []int, baseFrequency string) svPublisherGroupState {
	st := svPublisherGroupState{
		Mode:        "Manual",
		BalancedRMS: "0",
		TwoPhaseRMS: "0",
		Rows:        make(map[int]svPublisherRowState, len(channels)),
	}
	for i := 0; i < 3; i++ {
		st.CompRMS[i] = "0"
		st.CompPhase[i] = "0.00"
	}
	for _, ch := range channels {
		st.Rows[ch] = svPublisherRowState{
			RMS:       "0",
			Phase:     fmt.Sprintf("%.2f", defaultSVPublisherPhase(ch)),
			Frequency: baseFrequency,
			Quality:   formatSVPublisherQuality(sv.QualityValid),
		}
	}
	return st
}

func applySVPublisherGroupState(controls *svPublisherModeControls, state svPublisherGroupState) {
	if controls == nil {
		return
	}
	if state.BalancedRMS != "" {
		controls.balancedRMS.SetText(state.BalancedRMS)
	}
	if state.TwoPhaseRMS != "" {
		controls.twoPhaseRMS.SetText(state.TwoPhaseRMS)
	}
	for i := 0; i < 3; i++ {
		if state.CompRMS[i] != "" {
			controls.compRMS[i].SetText(state.CompRMS[i])
		}
		if state.CompPhase[i] != "" {
			controls.compPhase[i].SetText(state.CompPhase[i])
		}
	}
	for _, row := range controls.rows {
		if rowState, ok := state.Rows[row.channel]; ok {
			row.rms.SetText(rowState.RMS)
			row.phase.SetText(rowState.Phase)
			row.freq.SetText(rowState.Frequency)
			row.quality.SetText(rowState.Quality)
		}
	}
	if state.Mode != "" {
		controls.mode.SetSelected(state.Mode)
	}
	refreshSVPublisherModeControls(controls)
}

func captureSVPublisherManualState(state *svPublisherManualState, baseFreq *widget.Entry, simulation *widget.Check, calculateNeutral *widget.Check, syncSelect *widget.Select, useBaseFreq *widget.Check, currentControls, voltageControls *svPublisherModeControls) {
	if state == nil {
		return
	}
	state.BaseFrequency = baseFreq.Text
	state.UseBaseFrequency = useBaseFreq.Checked
	state.Simulation = simulation.Checked
	state.CalculateNeutral = calculateNeutral.Checked
	state.Sync = syncSelect.Selected
	state.Current = captureSVPublisherGroupState(currentControls)
	state.Voltage = captureSVPublisherGroupState(voltageControls)
}

func captureSVPublisherGroupState(controls *svPublisherModeControls) svPublisherGroupState {
	st := svPublisherGroupState{
		Mode:        "Manual",
		BalancedRMS: "0",
		TwoPhaseRMS: "0",
		Rows:        make(map[int]svPublisherRowState),
	}
	if controls == nil {
		return st
	}
	st.Mode = controls.mode.Selected
	st.BalancedRMS = controls.balancedRMS.Text
	st.TwoPhaseRMS = controls.twoPhaseRMS.Text
	for i := 0; i < 3; i++ {
		st.CompRMS[i] = controls.compRMS[i].Text
		st.CompPhase[i] = controls.compPhase[i].Text
	}
	for _, row := range controls.rows {
		st.Rows[row.channel] = svPublisherRowState{
			RMS:       row.rms.Text,
			Phase:     row.phase.Text,
			Frequency: row.freq.Text,
			Quality:   row.quality.Text,
		}
	}
	return st
}

func svPublisherSynchOptions() []string {
	return []string{"none", "local", "global"}
}

func svPublisherSynchOption(value sv.SmpSynch) string {
	switch value {
	case sv.SmpSynchLocal:
		return "local"
	case sv.SmpSynchGlobal:
		return "global"
	default:
		return "none"
	}
}

func svPublisherSynchValue(option string) sv.SmpSynch {
	switch option {
	case "local":
		return sv.SmpSynchLocal
	case "global":
		return sv.SmpSynchGlobal
	default:
		return sv.SmpSynchNone
	}
}

func wireSVPublisherAutoApply(schedule func(), baseFreq *widget.Entry, simulation *widget.Check, calculateNeutral *widget.Check, syncSelect *widget.Select, useBaseFreq *widget.Check, controls ...*svPublisherModeControls) {
	baseFreq.OnChanged = func(string) {
		for _, c := range controls {
			if useBaseFreq.Checked {
				setSVPublisherRowsFrequency(c.rows, baseFreq.Text)
			}
			refreshSVPublisherModeControls(c)
		}
		schedule()
	}
	simulation.OnChanged = func(bool) {
		schedule()
	}
	calculateNeutral.OnChanged = func(bool) {
		schedule()
	}
	syncSelect.OnChanged = func(string) {
		schedule()
	}
	useBaseFreq.OnChanged = func(v bool) {
		for _, c := range controls {
			if v {
				setSVPublisherRowsFrequency(c.rows, baseFreq.Text)
			}
			refreshSVPublisherModeControls(c)
		}
		schedule()
	}
	for _, c := range controls {
		c.mode.OnChanged = func(string) {
			refreshSVPublisherModeControls(c)
			schedule()
		}
		c.balancedRMS.OnChanged = func(string) {
			refreshSVPublisherModeControls(c)
			schedule()
		}
		c.twoPhaseRMS.OnChanged = func(string) {
			refreshSVPublisherModeControls(c)
			schedule()
		}
		for i := 0; i < 3; i++ {
			c.compRMS[i].OnChanged = func(string) {
				refreshSVPublisherModeControls(c)
				schedule()
			}
			c.compPhase[i].OnChanged = func(string) {
				refreshSVPublisherModeControls(c)
				schedule()
			}
		}
		for _, row := range c.rows {
			row.rms.OnChanged = func(string) {
				schedule()
			}
			row.phase.OnChanged = func(string) {
				schedule()
			}
			row.freq.OnChanged = func(string) {
				schedule()
			}
			row.quality.OnChanged = func(string) {
				schedule()
			}
		}
	}
}

func svPublisherModeOptions() []string {
	return []string{
		"Manual",
		"3-phase balanced",
		"2-phase AB",
		"2-phase BC",
		"2-phase CA",
		"Symmetrical components",
	}
}

func applySVPublisherMode(mode string, rows []svPublisherChannelRow, baseFreq string) {
	if mode == "" || mode == "Manual" {
		resetSVPublisherChannelLabels(rows)
		return
	}
	rms := firstNonZeroSVPublisherRMS(rows)
	active := make(map[int]bool)
	switch mode {
	case "3-phase balanced":
		active[0], active[1], active[2] = true, true, true
	case "2-phase AB":
		active[0], active[1] = true, true
	case "2-phase BC":
		active[1], active[2] = true, true
	case "2-phase CA":
		active[2], active[0] = true, true
	case "Symmetrical components":
		applySVPublisherSymmetricalLabels(rows)
		for i, row := range rows {
			if i < 3 {
				row.rms.SetText(rms)
				row.freq.SetText(baseFreq)
			} else {
				row.rms.SetText("0")
			}
			row.phase.SetText("0.00")
		}
		return
	default:
		return
	}

	resetSVPublisherChannelLabels(rows)
	for i, row := range rows {
		if active[i] {
			row.rms.SetText(rms)
		} else {
			row.rms.SetText("0")
		}
		row.phase.SetText(fmt.Sprintf("%.2f", defaultSVPublisherPhase(row.channel)))
		row.freq.SetText(baseFreq)
	}
}

func refreshSVPublisherModeControls(c *svPublisherModeControls) {
	if c == nil || c.mode == nil {
		return
	}
	resetSVPublisherChannelLabels(c.rows)
	c.balancedBox.Hide()
	c.twoPhaseBox.Hide()
	c.compBox.Hide()
	c.compPanel.Hide()

	switch c.mode.Selected {
	case "3-phase balanced":
		c.balancedBox.Show()
		setSVPublisherRowsEditable(c.rows, false)
		applySVPublisherBalancedDisplay(c.rows, c.balancedRMS.Text, c.baseFreq.Text)
	case "2-phase AB", "2-phase BC", "2-phase CA":
		c.twoPhaseBox.Show()
		setSVPublisherTwoPhaseRowsEditable(c.rows, c.mode.Selected)
		applySVPublisherTwoPhaseDisplay(c.rows, c.mode.Selected, c.twoPhaseRMS.Text, c.baseFreq.Text)
	case "Symmetrical components":
		c.compBox.Show()
		c.compPanel.Show()
		setSVPublisherRowsEditable(c.rows, false)
		applySVPublisherSymmetricalDisplay(c)
	default:
		setSVPublisherRowsEditable(c.rows, true)
		applySVPublisherMode(c.mode.Selected, c.rows, c.baseFreq.Text)
	}
	if c.bar != nil {
		c.bar.Refresh()
	}
}

func setSVPublisherRowsEditable(rows []svPublisherChannelRow, editable bool) {
	for _, row := range rows {
		setSVPublisherRowEditable(row, editable)
	}
}

func setSVPublisherRowEditable(row svPublisherChannelRow, editable bool) {
	if editable {
		row.rms.Enable()
		row.phase.Enable()
		row.freq.Enable()
	} else {
		row.rms.Disable()
		row.phase.Disable()
		row.freq.Disable()
	}
}

func setSVPublisherTwoPhaseRowsEditable(rows []svPublisherChannelRow, mode string) {
	active := svPublisherTwoPhaseActive(mode)
	for i, row := range rows {
		setSVPublisherRowEditable(row, !active[i])
	}
}

func applySVPublisherBalancedDisplay(rows []svPublisherChannelRow, rms, freq string) {
	for i, row := range rows {
		if i < 3 {
			row.rms.SetText(rms)
		} else {
			row.rms.SetText("0")
		}
		row.phase.SetText(fmt.Sprintf("%.2f", defaultSVPublisherPhase(row.channel)))
		row.freq.SetText(freq)
	}
}

func applySVPublisherTwoPhaseDisplay(rows []svPublisherChannelRow, mode, rms, freq string) {
	active := svPublisherTwoPhaseActive(mode)
	for i, row := range rows {
		if !active[i] {
			continue
		}
		row.rms.SetText(rms)
		row.phase.SetText(fmt.Sprintf("%.2f", defaultSVPublisherPhase(row.channel)))
		row.freq.SetText(freq)
	}
}

func svPublisherTwoPhaseActive(mode string) map[int]bool {
	active := make(map[int]bool)
	switch mode {
	case "2-phase AB":
		active[0], active[1] = true, true
	case "2-phase BC":
		active[1], active[2] = true, true
	case "2-phase CA":
		active[2], active[0] = true, true
	}
	return active
}

func applySVPublisherSymmetricalDisplay(c *svPublisherModeControls) {
	settings, err := svPublisherSymmetricalComponentSettings(c, parseSVPublisherFrequencyText(c.baseFreq.Text, c.nominalFreq))
	if err != nil {
		return
	}
	quality := sv.QualityValid
	phases := symmetricalComponentsToPhases(settings[0], settings[1], settings[2], settings[0].Frequency, quality)
	for i := 0; i < 3 && i < len(c.rows); i++ {
		c.rows[i].rms.SetText(fmt.Sprintf("%.3f", phases[i].RMS))
		c.rows[i].phase.SetText(fmt.Sprintf("%.2f", phases[i].PhaseDeg))
		c.rows[i].freq.SetText(fmt.Sprintf("%.2f", phases[i].Frequency))
	}
	if len(c.rows) > 3 {
		c.rows[3].rms.SetText("0")
		c.rows[3].phase.SetText("0.00")
		c.rows[3].freq.SetText(fmt.Sprintf("%.2f", settings[0].Frequency))
	}
}

func resetSVPublisherChannelLabels(rows []svPublisherChannelRow) {
	for _, row := range rows {
		row.label.SetText(sv.ChannelNames[row.channel])
	}
}

func applySVPublisherSymmetricalLabels(rows []svPublisherChannelRow) {
	if len(rows) < 4 {
		return
	}
	prefix := "I"
	if rows[0].channel >= sv.ChUa {
		prefix = "U"
	}
	rows[0].label.SetText(prefix + "1")
	rows[1].label.SetText(prefix + "2")
	rows[2].label.SetText(prefix + "0")
	rows[3].label.SetText(sv.ChannelNames[rows[3].channel])
}

func applySVPublisherRowsToSettings(settings *[sv.NumChannels]svpub.ChannelSetting, controls *svPublisherModeControls, baseFrequency float64, useBaseFreq bool, nominalFreq uint16) error {
	if controls == nil {
		return fmt.Errorf("mode controls not initialized")
	}
	switch controls.mode.Selected {
	case "3-phase balanced":
		return applySVPublisherBalancedRows(settings, controls, baseFrequency, useBaseFreq, nominalFreq)
	case "2-phase AB", "2-phase BC", "2-phase CA":
		return applySVPublisherTwoPhaseRows(settings, controls, baseFrequency, useBaseFreq, nominalFreq)
	case "Symmetrical components":
		return applySVPublisherSymmetricalRows(settings, controls, baseFrequency, useBaseFreq, nominalFreq)
	}
	for _, row := range controls.rows {
		setting, err := svPublisherRowSetting(row, baseFrequency, useBaseFreq, nominalFreq)
		if err != nil {
			return err
		}
		settings[row.channel] = setting
	}
	return nil
}

func applySVPublisherBalancedRows(settings *[sv.NumChannels]svpub.ChannelSetting, controls *svPublisherModeControls, baseFrequency float64, useBaseFreq bool, nominalFreq uint16) error {
	rms, err := parseFloatEntry(controls.balancedRMS, 0)
	if err != nil {
		return fmt.Errorf("ABC RMS: %w", err)
	}
	freq := baseFrequency
	if !useBaseFreq {
		freq = parseSVPublisherFrequencyText(controls.baseFreq.Text, nominalFreq)
	}
	for i, row := range controls.rows {
		quality, err := parseQualityHexEntry(row.quality)
		if err != nil {
			return fmt.Errorf("%s quality: %w", row.label.Text, err)
		}
		rowRMS := rms
		if i >= 3 {
			rowRMS = 0
		}
		settings[row.channel] = svpub.ChannelSetting{
			RMS:       rowRMS,
			PhaseDeg:  defaultSVPublisherPhase(row.channel),
			Frequency: freq,
			Quality:   quality,
		}
	}
	return nil
}

func applySVPublisherTwoPhaseRows(settings *[sv.NumChannels]svpub.ChannelSetting, controls *svPublisherModeControls, baseFrequency float64, useBaseFreq bool, nominalFreq uint16) error {
	rms, err := parseFloatEntry(controls.twoPhaseRMS, 0)
	if err != nil {
		return fmt.Errorf("two-phase RMS: %w", err)
	}
	freq := baseFrequency
	if !useBaseFreq {
		freq = parseSVPublisherFrequencyText(controls.baseFreq.Text, nominalFreq)
	}
	active := svPublisherTwoPhaseActive(controls.mode.Selected)
	for i, row := range controls.rows {
		if !active[i] {
			setting, err := svPublisherRowSetting(row, baseFrequency, useBaseFreq, nominalFreq)
			if err != nil {
				return err
			}
			settings[row.channel] = setting
			continue
		}
		quality, err := parseQualityHexEntry(row.quality)
		if err != nil {
			return fmt.Errorf("%s quality: %w", row.label.Text, err)
		}
		settings[row.channel] = svpub.ChannelSetting{
			RMS:       rms,
			PhaseDeg:  defaultSVPublisherPhase(row.channel),
			Frequency: freq,
			Quality:   quality,
		}
	}
	return nil
}

func svPublisherRowSetting(row svPublisherChannelRow, baseFrequency float64, useBaseFreq bool, nominalFreq uint16) (svpub.ChannelSetting, error) {
	rms, err := parseFloatEntry(row.rms, 0)
	if err != nil {
		return svpub.ChannelSetting{}, fmt.Errorf("%s RMS: %w", row.label.Text, err)
	}
	phase, err := parseFloatEntry(row.phase, defaultSVPublisherPhase(row.channel))
	if err != nil {
		return svpub.ChannelSetting{}, fmt.Errorf("%s phase: %w", row.label.Text, err)
	}
	freq := baseFrequency
	if !useBaseFreq {
		freq, err = parseFloatEntry(row.freq, float64(nominalFreq))
		if err != nil {
			return svpub.ChannelSetting{}, fmt.Errorf("%s frequency: %w", row.label.Text, err)
		}
	}
	quality, err := parseQualityHexEntry(row.quality)
	if err != nil {
		return svpub.ChannelSetting{}, fmt.Errorf("%s quality: %w", row.label.Text, err)
	}
	return svpub.ChannelSetting{
		RMS:       rms,
		PhaseDeg:  phase,
		Frequency: freq,
		Quality:   quality,
	}, nil
}

func applySVPublisherSymmetricalRows(settings *[sv.NumChannels]svpub.ChannelSetting, controls *svPublisherModeControls, baseFrequency float64, useBaseFreq bool, nominalFreq uint16) error {
	rows := controls.rows
	if len(rows) < 4 {
		return fmt.Errorf("symmetrical components require 4 rows")
	}
	freq := baseFrequency
	if !useBaseFreq {
		freq = parseSVPublisherFrequencyText(controls.baseFreq.Text, nominalFreq)
	}
	components, err := svPublisherSymmetricalComponentSettings(controls, freq)
	if err != nil {
		return err
	}
	phaseSettings := symmetricalComponentsToPhases(components[0], components[1], components[2], freq, sv.QualityValid)
	for i := 0; i < 3; i++ {
		quality, err := parseQualityHexEntry(rows[i].quality)
		if err != nil {
			return fmt.Errorf("%s quality: %w", rows[i].label.Text, err)
		}
		phaseSettings[i].Quality = quality
		settings[rows[i].channel] = phaseSettings[i]
	}
	neutralQuality, err := parseQualityHexEntry(rows[3].quality)
	if err != nil {
		return fmt.Errorf("%s quality: %w", rows[3].label.Text, err)
	}
	settings[rows[3].channel] = svpub.ChannelSetting{
		RMS:       0,
		PhaseDeg:  0,
		Frequency: freq,
		Quality:   neutralQuality,
	}
	return nil
}

func applySVPublisherCalculatedNeutral(settings *[sv.NumChannels]svpub.ChannelSetting) {
	settings[sv.ChIn] = calculateSVPublisherNeutral(settings[sv.ChIa], settings[sv.ChIb], settings[sv.ChIc])
	settings[sv.ChUn] = calculateSVPublisherNeutral(settings[sv.ChUa], settings[sv.ChUb], settings[sv.ChUc])
}

func calculateSVPublisherNeutral(a, b, c svpub.ChannelSetting) svpub.ChannelSetting {
	neutral := signal.Neutral(
		signal.Polar{Magnitude: a.RMS, AngleDeg: a.PhaseDeg},
		signal.Polar{Magnitude: b.RMS, AngleDeg: b.PhaseDeg},
		signal.Polar{Magnitude: c.RMS, AngleDeg: c.PhaseDeg},
	)
	frequency := a.Frequency
	if frequency <= 0 {
		frequency = b.Frequency
	}
	if frequency <= 0 {
		frequency = c.Frequency
	}
	return svpub.ChannelSetting{
		RMS:       neutral.Magnitude,
		PhaseDeg:  neutral.AngleDeg,
		Frequency: frequency,
		Quality:   a.Quality | b.Quality | c.Quality,
	}
}

func svPublisherSymmetricalComponentSettings(controls *svPublisherModeControls, freq float64) ([3]svpub.ChannelSetting, error) {
	var out [3]svpub.ChannelSetting
	for i := 0; i < 3; i++ {
		rms, err := parseFloatEntry(controls.compRMS[i], 0)
		if err != nil {
			return out, fmt.Errorf("%s%d RMS: %w", controls.prefix, []int{1, 2, 0}[i], err)
		}
		phase, err := parseFloatEntry(controls.compPhase[i], 0)
		if err != nil {
			return out, fmt.Errorf("%s%d phase: %w", controls.prefix, []int{1, 2, 0}[i], err)
		}
		out[i] = svpub.ChannelSetting{
			RMS:       rms,
			PhaseDeg:  phase,
			Frequency: freq,
			Quality:   sv.QualityValid,
		}
	}
	return out, nil
}

func parseSVPublisherFrequencyText(text string, nominalFreq uint16) float64 {
	freq, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(text), ",", "."), 64)
	if err != nil || freq <= 0 {
		return float64(nominalFreq)
	}
	return freq
}

func symmetricalComponentsToPhases(pos, neg, zero svpub.ChannelSetting, freq float64, quality sv.Quality) [3]svpub.ChannelSetting {
	phases := signal.SymmetricalComponentsToPhases(
		signal.Polar{Magnitude: pos.RMS, AngleDeg: pos.PhaseDeg},
		signal.Polar{Magnitude: neg.RMS, AngleDeg: neg.PhaseDeg},
		signal.Polar{Magnitude: zero.RMS, AngleDeg: zero.PhaseDeg},
	)
	var out [3]svpub.ChannelSetting
	for i, phase := range phases {
		out[i] = svpub.ChannelSetting{
			RMS:       phase.Magnitude,
			PhaseDeg:  phase.AngleDeg,
			Frequency: freq,
			Quality:   quality,
		}
	}
	return out
}

func firstNonZeroSVPublisherRMS(rows []svPublisherChannelRow) string {
	for _, row := range rows {
		text := strings.TrimSpace(row.rms.Text)
		if text == "" {
			continue
		}
		value, err := strconv.ParseFloat(strings.ReplaceAll(text, ",", "."), 64)
		if err == nil && value != 0 {
			return text
		}
	}
	return "0"
}

func setSVPublisherRowsFrequency(rows []svPublisherChannelRow, freq string) {
	for _, row := range rows {
		row.freq.SetText(freq)
	}
}

func formatSVPublisherQuality(q sv.Quality) string {
	return fmt.Sprintf("0x%08X", uint32(q))
}

func showSVPublisherQualityDialog(entry *widget.Entry, window fyne.Window) {
	current, err := parseQualityHexEntry(entry)
	if err != nil {
		showError(fmt.Errorf("quality: %w", err), window)
		return
	}

	hexValue := widget.NewEntry()
	hexValue.SetText(formatSVPublisherQuality(current))
	hexValue.Disable()

	validity := widget.NewRadioGroup([]string{"good", "invalid", "reserved", "questionable"}, nil)
	switch uint32(current) & 0x3 {
	case 1:
		validity.SetSelected("reserved")
	case 2:
		validity.SetSelected("invalid")
	case 3:
		validity.SetSelected("questionable")
	default:
		validity.SetSelected("good")
	}

	overflow := widget.NewCheck("overflow", nil)
	outOfRange := widget.NewCheck("outOfRange", nil)
	badReference := widget.NewCheck("badReference", nil)
	oscillatory := widget.NewCheck("oscillatory", nil)
	failure := widget.NewCheck("failure", nil)
	oldData := widget.NewCheck("oldData", nil)
	inconsistent := widget.NewCheck("inconsistent", nil)
	inaccurate := widget.NewCheck("inaccurate", nil)
	overflow.SetChecked(current&sv.QualityOverflow != 0)
	outOfRange.SetChecked(current&sv.QualityOutOfRange != 0)
	badReference.SetChecked(current&sv.QualityBadReference != 0)
	oscillatory.SetChecked(current&sv.QualityOscillatory != 0)
	failure.SetChecked(current&sv.QualityFailure != 0)
	oldData.SetChecked(current&sv.QualityOldData != 0)
	inconsistent.SetChecked(current&sv.QualityInconsistent != 0)
	inaccurate.SetChecked(current&sv.QualityInaccurate != 0)

	source := widget.NewRadioGroup([]string{"process", "substituted"}, nil)
	if current&sv.QualitySubstituted != 0 {
		source.SetSelected("substituted")
	} else {
		source.SetSelected("process")
	}

	test := widget.NewCheck("test", nil)
	operatorBlocked := widget.NewCheck("operatorBlocked", nil)
	derived := widget.NewCheck("derived", nil)
	test.SetChecked(current&sv.QualityTest != 0)
	operatorBlocked.SetChecked(current&sv.QualityOperatorBlocked != 0)
	derived.SetChecked(current&sv.QualityDerived != 0)

	content := container.NewVBox(
		widget.NewForm(widget.NewFormItem("Quality(hex)", hexValue)),
		widget.NewLabelWithStyle("Validity", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		validity,
		widget.NewLabelWithStyle("Detail quality", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		overflow,
		outOfRange,
		badReference,
		oscillatory,
		failure,
		oldData,
		inconsistent,
		inaccurate,
		widget.NewLabelWithStyle("Source", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		source,
		test,
		operatorBlocked,
		derived,
	)
	d := dialog.NewCustomConfirm("Quality flags", "OK", "Cancel", container.NewVScroll(content), func(ok bool) {
		if !ok {
			return
		}
		var q sv.Quality
		switch validity.Selected {
		case "invalid":
			q |= sv.QualityInvalid
		case "reserved":
			q |= sv.QualityReserved
		case "questionable":
			q |= sv.QualityQuestionable
		}
		if overflow.Checked {
			q |= sv.QualityOverflow
		}
		if outOfRange.Checked {
			q |= sv.QualityOutOfRange
		}
		if badReference.Checked {
			q |= sv.QualityBadReference
		}
		if oscillatory.Checked {
			q |= sv.QualityOscillatory
		}
		if failure.Checked {
			q |= sv.QualityFailure
		}
		if oldData.Checked {
			q |= sv.QualityOldData
		}
		if inconsistent.Checked {
			q |= sv.QualityInconsistent
		}
		if inaccurate.Checked {
			q |= sv.QualityInaccurate
		}
		if source.Selected == "substituted" {
			q |= sv.QualitySubstituted
		}
		if test.Checked {
			q |= sv.QualityTest
		}
		if operatorBlocked.Checked {
			q |= sv.QualityOperatorBlocked
		}
		if derived.Checked {
			q |= sv.QualityDerived
		}
		entry.SetText(formatSVPublisherQuality(q))
	}, window)
	d.Resize(fyne.NewSize(520, 620))
	d.Show()
}

func defaultSVPublisherPhase(ch int) float64 {
	switch ch {
	case sv.ChIb, sv.ChUb:
		return 240
	case sv.ChIc, sv.ChUc:
		return 120
	default:
		return 0
	}
}

func parseFloatEntry(entry *widget.Entry, fallback float64) (float64, error) {
	text := strings.TrimSpace(entry.Text)
	if text == "" {
		return fallback, nil
	}
	return strconv.ParseFloat(strings.ReplaceAll(text, ",", "."), 64)
}

func parseQualityHexEntry(entry *widget.Entry) (sv.Quality, error) {
	text := strings.TrimSpace(entry.Text)
	if text == "" {
		return sv.QualityValid, nil
	}
	value, err := strconv.ParseUint(text, 0, 32)
	if err != nil {
		return sv.QualityValid, err
	}
	if sv.Quality(value)&^sv.QualityMask != 0 {
		return sv.QualityValid, fmt.Errorf("unsupported bits outside IEC 61850 Quality mask 0x%08X", uint32(sv.QualityMask))
	}
	return sv.Quality(value), nil
}
