package presentation

import (
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
)

type gooseViewRuntime struct {
	control.Controller
	mu          sync.Mutex
	snapshots   []goosesub.Snapshot
	subs, stops int
}

func (r *gooseViewRuntime) Status(id control.ModuleID) control.ModuleStatus {
	return control.ModuleStatus{ID: id, State: control.StatusRunning}
}
func (r *gooseViewRuntime) Subscribe(int) (<-chan control.Event, func()) {
	r.mu.Lock()
	r.subs++
	r.mu.Unlock()
	ch := make(chan control.Event)
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.stops++
			close(ch)
		})
	}
}
func (r *gooseViewRuntime) GooseSubscriberSnapshots(name string) ([]goosesub.Snapshot, control.ModuleStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []goosesub.Snapshot
	if name == "events" {
		for _, v := range r.snapshots {
			v.PDU = goose.ClonePDU(v.PDU)
			out = append(out, v)
		}
	}
	return out, r.Status(control.ModuleGooseSub)
}

func gooseViewSnapshot(value bool) goosesub.Snapshot {
	return goosesub.Snapshot{ReceivedAt: time.Now(), PDU: &goose.PDU{
		StNum: 7, SqNum: 20, ConfRev: 1, AppID: 1, DatSet: "IED/LLN0$events", NumDatSetEntries: 1,
		AllData: []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: value}},
	}}
}

func subscriberViewText(obj fyne.CanvasObject) string {
	var text []string
	var visit func(fyne.CanvasObject)
	visit = func(obj fyne.CanvasObject) {
		switch x := obj.(type) {
		case *fyne.Container:
			for _, c := range x.Objects {
				visit(c)
			}
		case *container.Scroll:
			visit(x.Content)
		case *widget.Card:
			visit(x.Content)
		case *widget.Label:
			text = append(text, x.Text)
		case *widget.Accordion:
			for _, item := range x.Items {
				text = append(text, item.Title)
			}
		}
	}
	visit(obj)
	return strings.Join(text, "\n")
}

func TestGooseSubscriberRestoresSnapshotAfterModuleSwitch(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	r := &gooseViewRuntime{snapshots: []goosesub.Snapshot{gooseViewSnapshot(true)}}
	s := &uiState{app: app, window: app.NewWindow("Manual"), configPath: t.TempDir(), runtime: r,
		manualPane: container.NewStack(), cfg: &config.Config{GooseSub: config.GooseSubCfg{
			Enabled: true, Subscriptions: []config.GooseSubscriber{{Name: "events"}},
		}}}
	defer func() {
		if s.manualStop != nil {
			s.manualStop()
		}
	}()
	for range 2 {
		s.showManualModule(moduleGooseSub)
		// No GUI event is ever sent by this fake runtime, including heartbeats.
		// Values must be loaded synchronously from accepted receiver state.
		s.manualStop()
		if text := subscriberViewText(s.manualPane); !strings.Contains(text, "Entry1: true") || !strings.Contains(text, "live") || !strings.Contains(text, "sqNum: 20") {
			t.Fatal("returning view lost accepted values", text)
		}
		s.showManualModule(moduleGoosePub)
		s.manualStop()
	}
	// Changes received while the view was closed must replace the old view's
	// values, rather than restoring a stale UI-only cache.
	r.mu.Lock()
	r.snapshots = []goosesub.Snapshot{gooseViewSnapshot(false)}
	r.mu.Unlock()
	s.showManualModule(moduleGooseSub)
	s.manualStop()
	if text := subscriberViewText(s.manualPane); !strings.Contains(text, "Entry1: false") || strings.Contains(text, "Entry1: true") {
		t.Fatal("reopening restored a stale UI cache", text)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subs != 3 || r.stops != 3 {
		t.Fatalf("view leaked subscriptions: subscribed=%d cancelled=%d", r.subs, r.stops)
	}
}

func newSnapshotTestPanel() *gooseSubscriberPanel {
	a := widget.NewAccordion()
	empty := widget.NewLabel("No decoded Dataset values yet.")
	return &gooseSubscriberPanel{status: widget.NewLabel(""), stream: widget.NewLabel(""), flags: widget.NewLabel(""),
		modelStatus: widget.NewLabel(""), accordion: a, empty: empty, dataset: container.NewVBox(empty, a), treeOpen: map[int]map[string]bool{}}
}

func TestGooseSubscriberSnapshotFreshnessAndLifecycle(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	p := newSnapshotTestPanel()
	v := gooseViewSnapshot(true)
	running := control.ModuleStatus{ID: control.ModuleGooseSub, State: control.StatusRunning}
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, running)
	item := p.accordion.Items[0]
	item.Open = true
	v.PDU.SqNum++
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, running)
	if p.accordion.Items[0] != item || !item.Open || !strings.Contains(p.flags.Text, "sqNum: 21") {
		t.Fatal("heartbeat recreated signal widgets or failed to refresh metadata")
	}
	v.Stale = true
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, running)
	if !strings.Contains(p.status.Text, "stale") || p.accordion.Items[0].Title != "Entry1: true" {
		t.Fatal("stale values disappeared or were shown as live", p.status.Text)
	}
	v.Stale = false
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, control.ModuleStatus{State: control.StatusStopped})
	if !strings.Contains(p.status.Text, "Stopped") || strings.Contains(p.status.Text, "live") {
		t.Fatal("stopped module reported live data", p.status.Text)
	}
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, control.ModuleStatus{State: control.StatusError, Error: "capture failed"})
	if !strings.Contains(p.status.Text, "capture failed") || strings.Contains(p.status.Text, "live") {
		t.Fatal("module error hidden", p.status.Text)
	}
	updateGooseSubscriberSnapshots(p, nil, running)
	if len(p.accordion.Items) != 0 || !p.empty.Visible() || !strings.Contains(p.status.Text, "waiting") {
		t.Fatal("new runtime retained values from an old instance")
	}
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{{}}, running)
	if len(p.accordion.Items) != 0 {
		t.Fatal("nil PDU populated the view")
	}
}

func TestGooseSubscriberSnapshotValidatesICDAndFlagsAmbiguity(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	_, desc := subscriberICDFixture(t)
	p := newSnapshotTestPanel()
	p.description = &desc
	v := gooseViewSnapshot(true)
	v.PDU.ConfRev = desc.ConfRev + 1
	running := control.ModuleStatus{State: control.StatusRunning}
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{v}, running)
	if p.accordion.Items[0].Title != "Entry1: true" || !strings.Contains(p.modelStatus.Text, "mismatch") {
		t.Fatal("snapshot bypassed ICD validation")
	}
	newer := gooseViewSnapshot(false)
	newer.ReceivedAt = v.ReceivedAt.Add(time.Second)
	updateGooseSubscriberSnapshots(p, []goosesub.Snapshot{newer, v}, running)
	if p.accordion.Items[0].Title != "Entry1: false" || !strings.Contains(p.status.Text, "2 matching streams") {
		t.Fatal("ambiguous subscription did not identify the latest snapshot")
	}
}
