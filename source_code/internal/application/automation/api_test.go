package automation

import (
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pbmt/internal/application/catalog"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/application/svpub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
	"pbmt/profiles"
)

type memoryAgent struct {
	control.Agent
	goose                 goosepub.Snapshot
	sv                    svpub.Snapshot
	gooseIn               []goosesub.Snapshot
	svIn                  []svsub.Snapshot
	state                 control.Status
	gooseWrites, svWrites int
	closes, releases      int
}

func (m *memoryAgent) GooseOutput(string) (goosepub.Snapshot, control.ModuleStatus, error) {
	v := m.goose
	v.Data = goose.CloneDataValues(v.Data)
	return v, control.ModuleStatus{ID: control.ModuleGoosePub, State: m.state}, nil
}
func (m *memoryAgent) SVOutput(string) (svpub.Snapshot, control.ModuleStatus, error) {
	return m.sv, control.ModuleStatus{ID: control.ModuleSVPub, State: m.state}, nil
}
func (m *memoryAgent) GooseInput(string) ([]goosesub.Snapshot, control.ModuleStatus, error) {
	return m.gooseIn, control.ModuleStatus{ID: control.ModuleGooseSub, State: m.state}, nil
}
func (m *memoryAgent) SVInput(string) ([]svsub.Snapshot, control.ModuleStatus, error) {
	return m.svIn, control.ModuleStatus{ID: control.ModuleSVSub, State: m.state}, nil
}
func (m *memoryAgent) ApplyGoose(_ string, v []goose.DataValue, test, sim bool) (bool, error) {
	m.gooseWrites++
	m.goose.Data = goose.CloneDataValues(v)
	m.goose.Test = test
	m.goose.Simulation = sim
	return true, nil
}
func (m *memoryAgent) ApplySV(_ string, v [sv.NumChannels]svpub.ChannelSetting, sim bool, synch sv.SmpSynch) error {
	m.svWrites++
	m.sv.Settings = v
	m.sv.Simulation = sim
	m.sv.Synch = synch
	return nil
}
func (m *memoryAgent) Close() error   { m.closes++; return nil }
func (m *memoryAgent) Release() error { m.releases++; return nil }

func TestReleaseRevokesWithoutClosingPublishers(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_pub")
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if m.releases != 1 || m.closes != 0 {
		t.Fatal("wrong release policy")
	}
	if _, err := a.GooseSet(GooseSetRequest{st.ID, a.Catalog.Revision, []GooseChange{{st.Signals[0].ID, json.RawMessage("true")}}}); err == nil || m.gooseWrites != 0 {
		t.Fatal("released API accepted write")
	}
	if err := a.Close(); err != nil || m.closes != 0 {
		t.Fatal("late close stopped released outputs")
	}
}

func testAPI(t *testing.T) (*API, *memoryAgent) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := profiles.CreateProject(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Prepare(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &memoryAgent{state: control.StatusRunning, goose: goosepub.Snapshot{Test: true, Simulation: true, ChangedAt: time.Now()}, sv: svpub.Snapshot{Ready: true, Simulation: true, Synch: sv.SmpSynchGlobal}}
	for _, sig := range streamFor(t, a, "goose_pub").Signals {
		raw := json.RawMessage("false")
		if sig.Type == "Quality" {
			raw = json.RawMessage("0")
		}
		v, err := decodeGooseValue(sig.Type, raw)
		if err != nil {
			t.Fatal(err)
		}
		m.goose.Data = append(m.goose.Data, v)
	}
	for i := range m.sv.Settings {
		m.sv.Settings[i] = svpub.ChannelSetting{RMS: float64(i + 1), PhaseDeg: float64(i * 30), Frequency: 50, Quality: sv.Quality(3)}
	}
	a.Attach(m)
	return a, m
}
func streamFor(t *testing.T, a *API, module string) catalog.Stream {
	t.Helper()
	for _, m := range a.Catalog.Modules {
		if m.ID == module && len(m.Streams) > 0 {
			return m.Streams[0]
		}
	}
	t.Fatal("missing stream", module)
	return catalog.Stream{}
}

func TestGooseBatchValidationIsAtomicAndPreservesFlags(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_pub")
	req := GooseSetRequest{StreamID: st.ID, Revision: a.Catalog.Revision, Changes: []GooseChange{{st.Signals[0].ID, json.RawMessage("true")}, {st.Signals[1].ID, json.RawMessage("\"bad\"")}}}
	if _, err := a.GooseSet(req); err == nil || m.gooseWrites != 0 || m.goose.Data[0].Bool {
		t.Fatal("invalid batch partly applied")
	}
	req.Changes = req.Changes[:1]
	result, err := a.GooseSet(req)
	if err != nil || !result.Applied || !m.goose.Data[0].Bool || !m.goose.Test || !m.goose.Simulation {
		t.Fatalf("valid batch: %+v %v", result, err)
	}
	req.Changes = append(req.Changes, req.Changes[0])
	if _, err := a.GooseSet(req); err == nil {
		t.Fatal("duplicate accepted")
	}
	req.Changes = req.Changes[:1]
	req.Revision = "old"
	if _, err := a.GooseSet(req); err == nil {
		t.Fatal("stale revision accepted")
	}
	req.Revision = a.Catalog.Revision
	m.state = control.StatusStopped
	if _, err := a.GooseSet(req); err == nil {
		t.Fatal("stopped publisher accepted")
	}
	if m.gooseWrites != 1 {
		t.Fatal("rejected commands changed output")
	}
}

func TestGooseScalarTypesAndLimits(t *testing.T) {
	for _, tc := range []struct {
		kind, raw string
		valid     bool
	}{
		{"UNSIGNED", "18446744073709551615", true}, {"INTEGER", "-9223372036854775808", true}, {"INTEGER", "9223372036854775808", false},
		{"BOOLEAN", "1", false}, {"BOOLEAN", "null", false}, {"FLOAT", "1e100", false}, {"FLOAT", "3.14", true},
		{"UTC_TIME", "\"2026-09-16T12:00:00Z\"", true}, {"UTC_TIME", "\"2200-01-01T00:00:00Z\"", false}, {"Quality", "16384", false},
	} {
		v, err := decodeGooseValue(tc.kind, json.RawMessage(tc.raw))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s: %v", tc.kind, tc.raw, err)
		}
		if tc.kind == "UNSIGNED" && tc.valid && v.UInt != math.MaxUint64 {
			t.Fatal("integer precision lost")
		}
	}
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_pub")
	st.Signals[0].Type = "VISIBLE_STRING"
	a.streams[st.ID] = st
	raw, _ := json.Marshal(strings.Repeat("x", 1600))
	if _, err := a.GooseSet(GooseSetRequest{st.ID, a.Catalog.Revision, []GooseChange{{st.Signals[0].ID, raw}}}); err == nil || m.gooseWrites != 0 {
		t.Fatal("oversized Ethernet payload accepted")
	}
}

func TestSVPartialUpdatePreservesEngineerSettings(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "sv_pub")
	before := m.sv
	rms, phase := 4.0, -120.0
	req := SVSetRequest{st.ID, a.Catalog.Revision, []SVChange{{st.Signals[0].ID, &rms, &phase}}}
	result, err := a.SVSet(req)
	if err != nil || !result.Applied || m.sv.Settings[0].RMS != 4 || m.sv.Settings[0].PhaseDeg != phase {
		t.Fatalf("set: %+v %v", result, err)
	}
	for i, v := range m.sv.Settings {
		if v.Frequency != before.Settings[i].Frequency || v.Quality != before.Settings[i].Quality || (i != 0 && v != before.Settings[i]) {
			t.Fatal("unrequested generator field changed")
		}
	}
	if m.sv.Synch != before.Synch || m.sv.Simulation != before.Simulation {
		t.Fatal("protocol flags changed")
	}
	bad := math.Inf(1)
	req.Changes = append(req.Changes, SVChange{st.Signals[1].ID, &bad, nil})
	if _, err := a.SVSet(req); err == nil || m.svWrites != 1 {
		t.Fatal("invalid batch applied")
	}
	req.Changes = req.Changes[:1]
	huge := 1e100
	req.Changes[0].RMS = &huge
	if _, err := a.SVSet(req); err == nil || m.svWrites != 1 {
		t.Fatal("out-of-range waveform applied")
	}
	req.Changes[0].RMS = &rms
	m.sv.Ready = false
	if _, err := a.SVSet(req); err == nil {
		t.Fatal("unprepared waveform accepted")
	}
}

func TestInputMissingStaleAndUnresolvedAreExplicit(t *testing.T) {
	a, m := testAPI(t)
	gs := streamFor(t, a, "goose_sub")
	ss := streamFor(t, a, "sv_sub")
	g, err := a.GooseGet(StreamRequest{gs.ID})
	if err != nil || g.Available || g.Fresh || len(g.Values) != 0 {
		t.Fatal("missing GOOSE fabricated", g, err)
	}
	stats := &svsub.StreamStats{}
	stats.Angle[0] = math.NaN()
	m.svIn = []svsub.Snapshot{{ReceivedAt: time.Now(), MeasuredAt: time.Now(), Stats: stats, Stale: true}}
	v, err := a.SVGet(StreamRequest{ss.ID})
	if err != nil || !v.Available || v.Fresh || v.Channels[0].RMS != nil || v.Channels[0].Phase != nil || v.Channels[0].Frequency != nil {
		t.Fatal("missing SV measurement fabricated", v, err)
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatal("NaN leaked into JSON", err)
	}
	m.gooseIn = []goosesub.Snapshot{{PDU: &goose.PDU{AllData: []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}}, ReceivedAt: time.Now()}}
	g, err = a.GooseGet(StreamRequest{gs.ID})
	if err != nil || !g.Fresh || g.SchemaMatched || g.Values[0].ID != "" {
		t.Fatal("unresolved values mislabeled", g, err)
	}
	m.state = control.StatusStopped
	g, err = a.GooseGet(StreamRequest{gs.ID})
	if err != nil || g.Fresh {
		t.Fatal("stopped receiver fresh", g, err)
	}
	m.gooseIn = append(m.gooseIn, m.gooseIn[0])
	if _, err := a.GooseGet(StreamRequest{gs.ID}); err == nil {
		t.Fatal("ambiguous stream silently selected")
	}
}

func TestSourceChangesRevokeCommandsAndDecodeRejectsTypos(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_pub")
	path := filepath.Join(a.root, "cfg", "goose_sub.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n# external edit\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GooseSet(GooseSetRequest{st.ID, a.Catalog.Revision, []GooseChange{{st.Signals[0].ID, json.RawMessage("true")}}}); err == nil || m.gooseWrites != 0 {
		t.Fatal("command applied after source edit")
	}
	for _, raw := range []string{`{"stream_id":"a","typo":1}`, `null`, `{} {}`, `[]`} {
		var req StreamRequest
		if Decode([]byte(raw), &req) == nil {
			t.Fatal("malformed arguments accepted", raw)
		}
	}
}

func TestRevocationRejectsCommandsQueuedBeforeClose(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_pub")
	a.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := a.GooseSet(GooseSetRequest{st.ID, a.Catalog.Revision, []GooseChange{{st.Signals[0].ID, json.RawMessage("true")}}})
		done <- err
	}()
	a.Revoke()
	a.mu.Unlock()
	if err := <-done; err == nil || m.gooseWrites != 0 {
		t.Fatal("queued command applied after revocation")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGooseNamesRequireMatchingICDRevisionAndStructure(t *testing.T) {
	a, m := testAPI(t)
	st := streamFor(t, a, "goose_sub")
	st.Signals = []catalog.Signal{{ID: st.ID + "/entry/0", Name: "Trip.stVal", Type: "BOOLEAN"}}
	a.streams[st.ID] = st
	a.descriptions[st.ID] = &sclmodel.GooseStream{GocbRef: "IED/LLN0$GO$Trip", DatSet: "IED/LLN0$TripData", DstMAC: "01:0c:cd:01:00:01", AppID: 1, ConfRev: 2,
		Entries: []*sclmodel.Type{{Name: "Trip.stVal", Kind: "BOOLEAN"}}}
	mac, _ := net.ParseMAC("01:0c:cd:01:00:01")
	pdu := &goose.PDU{GocbRef: "IED/LLN0$GO$Trip", DatSet: "IED/LLN0$TripData", DstMAC: mac, AppID: 1, ConfRev: 2, NumDatSetEntries: 1,
		AllData: []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}}
	m.gooseIn = []goosesub.Snapshot{{PDU: pdu, ReceivedAt: time.Now()}}
	v, err := a.GooseGet(StreamRequest{st.ID})
	if err != nil || !v.SchemaMatched || v.Values[0].ID != st.Signals[0].ID || v.Values[0].Name != "Trip.stVal" {
		t.Fatal("matching ICD not used", v, err)
	}
	pdu.ConfRev++
	v, err = a.GooseGet(StreamRequest{st.ID})
	if err != nil || v.SchemaMatched || v.Values[0].ID != "" || v.Values[0].Name != "Entry1" {
		t.Fatal("mismatched ICD mislabeled live data", v, err)
	}
}
