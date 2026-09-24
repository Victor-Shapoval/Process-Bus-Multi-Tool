package presentation

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

func TestManualSVLoadsAppliedWaveformWithoutRounding(t *testing.T) {
	var snapshot svpub.Snapshot
	snapshot.Ready = true
	snapshot.Simulation = true
	snapshot.Synch = sv.SmpSynchGlobal
	for ch := range snapshot.Settings {
		snapshot.Settings[ch] = svpub.ChannelSetting{RMS: 127017.05922171767 + float64(ch), PhaseDeg: float64(ch) * 33.25, Frequency: 50 + float64(ch), Quality: sv.Quality(ch)}
	}
	state := &svPublisherManualState{UseBaseFrequency: true, CalculateNeutral: true}
	loadSVPublisherSnapshot(state, snapshot)
	if state.UseBaseFrequency || state.CalculateNeutral || state.Current.Mode != "Manual" || state.Voltage.Mode != "Manual" || !state.Simulation || state.Sync != svPublisherSynchOption(snapshot.Synch) {
		t.Fatal("handover left automatic waveform transformations enabled")
	}
	for ch, want := range snapshot.Settings {
		row := state.Current.Rows[ch]
		if ch > sv.ChIn {
			row = state.Voltage.Rows[ch]
		}
		for text, value := range map[string]float64{row.RMS: want.RMS, row.Phase: want.PhaseDeg, row.Frequency: want.Frequency} {
			got, err := strconv.ParseFloat(text, 64)
			if err != nil || got != value {
				t.Fatalf("handover rounded value: %q != %v", text, value)
			}
		}
		if row.Quality != formatSVPublisherQuality(want.Quality) {
			t.Fatal("quality changed")
		}
	}
}

func TestManualGOOSEPreservesUneditedRuntimeValues(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	entries := []config.GooseDatasetEntry{{Name: "b", Type: "bool"}, {Name: "q", Type: "quality"}, {Name: "i", Type: "int"}, {Name: "u", Type: "uint"}, {Name: "f", Type: "float"}, {Name: "s", Type: "string"}, {Name: "t", Type: "utc_time"}}
	values := []goose.DataValue{
		{Type: goose.DataTypeBoolean, Bool: true}, goose.NewQualityBitString(1<<11 | 1<<13),
		{Type: goose.DataTypeInteger, Int: -42}, {Type: goose.DataTypeUnsigned, UInt: 1 << 40},
		{Type: goose.DataTypeFloatingPoint, Float: 12.5}, {Type: goose.DataTypeVisibleString, String: "  retain spaces  "},
		{Type: goose.DataTypeUTCTime, Time: time.Unix(1700000000, 123456789).UTC(), TimeQuality: goose.TimeQuality{Accuracy: 10}},
	}
	controls, _ := newGoosePublisherDatasetGrid(entries, app.NewWindow("handover"))
	loadGoosePublisherValues(controls, values)
	got, err := goosePublisherDataFromControls(controls)
	if err != nil || !reflect.DeepEqual(got, values) {
		t.Fatalf("handover changed values: %+v, %v", got, err)
	}
	controls[0].boolVal.SetChecked(false)
	got, err = goosePublisherDataFromControls(controls)
	if err != nil || got[0].Bool || !reflect.DeepEqual(got[1:], values[1:]) {
		t.Fatal("manual edit changed unrelated fields", err)
	}
}
