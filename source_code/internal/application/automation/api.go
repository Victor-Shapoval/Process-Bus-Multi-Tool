// Package automation provides project-scoped operations shared by MCP tools.
// It has no transport or GUI dependencies and never configures protocol timing.
package automation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pbmt/internal/application/catalog"
	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

type ServerOptions struct {
	Address string
	Token   string
}
type ServerStatus struct {
	Running   bool
	Connected bool
	Address   string
	Error     string
}
type Server interface {
	Status() ServerStatus
	Stop() error
}

type API struct {
	mu                sync.Mutex
	revoked           atomic.Bool
	root, fingerprint string
	Catalog           *catalog.Catalog
	agent             control.Agent
	streams           map[string]catalog.Stream
	descriptions      map[string]*sclmodel.GooseStream
	gooseHeaders      map[string]goose.PDU
	sequences         map[string]*sequenceRun
	nextSequence      uint64
	tests             map[string]*testRun
	nextTest          uint64
}

// Prepare freezes the exact catalog used for control. Source edits invalidate
// this instance; they never silently retarget signal IDs in a live session.
func Prepare(root string, cfg *config.Config) (*API, error) {
	before, err := catalog.SourcesFingerprint(root)
	if err != nil {
		return nil, err
	}
	c, err := catalog.Refresh(root, cfg)
	if err != nil {
		return nil, err
	}
	if c.ConfigurationState != "matches_applied" {
		return nil, errors.New("saved and applied configurations differ; apply/reload the project before starting MCP")
	}
	a := &API{root: root, fingerprint: before, Catalog: c, streams: map[string]catalog.Stream{}, descriptions: map[string]*sclmodel.GooseStream{}, gooseHeaders: map[string]goose.PDU{}}
	for _, pub := range cfg.GoosePub.Publishers {
		// Reserve maximum counter lengths; retransmissions must continue to fit
		// standard Ethernet MTU after the immediate state-change frame.
		a.gooseHeaders[pub.Name] = goose.PDU{GocbRef: pub.GocbRef, DatSet: pub.DatSet, GoID: pub.GoID,
			ConfRev: pub.ConfRev, TimeAllowedToLiveMs: math.MaxUint32, StNum: math.MaxUint32, SqNum: math.MaxUint32}
	}
	gs, _ := sclmodel.ProjectGooseDescriptions(root)
	for _, module := range c.Modules {
		for _, stream := range module.Streams {
			a.streams[stream.ID] = stream
		}
	}
	for _, sub := range cfg.GooseSub.Subscriptions {
		for id, stream := range a.streams {
			if strings.HasPrefix(id, "goose_sub/") && stream.Name == sub.Name {
				a.descriptions[id], _ = sclmodel.GooseDescription(sub, gs)
			}
		}
	}
	if err := a.CheckSources(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *API) Attach(agent control.Agent) { a.agent = agent }

// Revoke fences queued commands immediately, before shutdown waits for an
// operation already in progress. Close then stops the publisher runtimes.
func (a *API) Revoke() { a.revoked.Store(true) }

func (a *API) Close() error {
	return a.release(true)
}

// Release fences MCP writes and returns the unchanged outputs to Manual.
func (a *API) Release() error {
	return a.release(false)
}

func (a *API) release(stopPublishers bool) error {
	a.Revoke()
	a.mu.Lock()
	var waiting []<-chan struct{}
	for _, run := range a.tests {
		run.cancel()
		waiting = append(waiting, run.done)
	}
	for _, run := range a.sequences {
		a.finishSequence(run, "cancelled", "MCP control released")
	}
	a.mu.Unlock()
	// Tests reset their outputs while the lease is still valid. Never hold
	// API.mu while joining a worker that needs it to publish its final result.
	for _, done := range waiting {
		<-done
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agent == nil {
		return nil
	}
	var err error
	if stopPublishers {
		err = a.agent.Close()
	} else {
		err = a.agent.Release()
	}
	a.agent = nil
	return err
}

func (a *API) CheckSources() error {
	now, err := catalog.SourcesFingerprint(a.root)
	if err != nil {
		return fmt.Errorf("project source check failed: %w", err)
	}
	if now != a.fingerprint {
		return errors.New("project configuration/ICD changed; stop MCP and reload the project")
	}
	return nil
}

func (a *API) ready() error {
	if a.agent == nil || a.revoked.Load() {
		return errors.New("MCP control is not active")
	}
	return a.CheckSources()
}

type StreamRequest struct {
	StreamID string `json:"stream_id"`
}
type GooseChange struct {
	SignalID string          `json:"signal_id"`
	Value    json.RawMessage `json:"value"`
}
type GooseSetRequest struct {
	StreamID string        `json:"stream_id"`
	Revision string        `json:"catalog_revision"`
	Changes  []GooseChange `json:"changes"`
}
type SVChange struct {
	SignalID string   `json:"signal_id"`
	RMS      *float64 `json:"rms,omitempty"`
	Phase    *float64 `json:"phase_deg,omitempty"`
}
type SVSetRequest struct {
	StreamID string     `json:"stream_id"`
	Revision string     `json:"catalog_revision"`
	Changes  []SVChange `json:"changes"`
}
type Applied struct {
	Applied         bool   `json:"applied"`
	Changed         bool   `json:"changed"`
	CatalogRevision string `json:"catalog_revision"`
	Note            string `json:"note"`
}
type SignalValue struct {
	ID       string        `json:"signal_id,omitempty"`
	Name     string        `json:"name"`
	Type     string        `json:"type"`
	Value    any           `json:"value"`
	Children []SignalValue `json:"children,omitempty"`
}
type GooseState struct {
	StreamID        string               `json:"stream_id"`
	CatalogRevision string               `json:"catalog_revision"`
	Direction       string               `json:"direction"`
	Module          control.ModuleStatus `json:"module"`
	Available       bool                 `json:"available"`
	Fresh           bool                 `json:"fresh"`
	ReceivedAt      *time.Time           `json:"received_at,omitempty"`
	ChangedAt       *time.Time           `json:"changed_at,omitempty"`
	SchemaMatched   bool                 `json:"schema_matched"`
	Warning         string               `json:"warning,omitempty"`
	Values          []SignalValue        `json:"values"`
	Test            bool                 `json:"test"`
	Simulation      bool                 `json:"simulation"`
}
type ChannelValue struct {
	ID            string   `json:"signal_id"`
	Name          string   `json:"name"`
	Unit          string   `json:"unit"`
	RMS           *float64 `json:"rms"`
	Phase         *float64 `json:"phase_deg"`
	Frequency     *float64 `json:"frequency_hz"`
	Instantaneous *float64 `json:"instantaneous,omitempty"`
	Quality       uint32   `json:"quality"`
}
type SVState struct {
	StreamID        string               `json:"stream_id"`
	CatalogRevision string               `json:"catalog_revision"`
	Direction       string               `json:"direction"`
	Module          control.ModuleStatus `json:"module"`
	Available       bool                 `json:"available"`
	Fresh           bool                 `json:"fresh"`
	ReceivedAt      *time.Time           `json:"received_at,omitempty"`
	MeasuredAt      *time.Time           `json:"measured_at,omitempty"`
	Basis           string               `json:"basis"`
	PhaseReference  string               `json:"phase_reference"`
	Warning         string               `json:"warning,omitempty"`
	Channels        []ChannelValue       `json:"channels"`
}

func (a *API) GetCatalog() (*catalog.Catalog, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ready(); err != nil {
		return nil, err
	}
	return a.Catalog, nil
}

func (a *API) GetStatus() (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.agent == nil {
		return nil, errors.New("MCP control is not active")
	}
	states, err := a.agent.Statuses()
	if err != nil {
		return nil, err
	}
	return struct {
		Project         string                 `json:"project"`
		ControlOwner    string                 `json:"control_owner"`
		CatalogRevision string                 `json:"catalog_revision"`
		Modules         []control.ModuleStatus `json:"modules"`
		Sequences       []SequenceStatus       `json:"sequences"`
		Tests           []TestStatus           `json:"tests"`
	}{a.Catalog.Project, "mcp", a.Catalog.Revision, states, a.sequenceStatuses(), a.testStatuses()}, nil
}

func (a *API) stream(id, protocol string, outputOnly bool) (catalog.Stream, error) {
	s, ok := a.streams[id]
	if !ok || (!strings.HasPrefix(id, protocol+"_pub/") && (!strings.HasPrefix(id, protocol+"_sub/") || outputOnly)) {
		return s, errors.New("unknown stream or wrong protocol/direction; use get_catalog")
	}
	return s, nil
}

func (a *API) GooseGet(req StreamRequest) (GooseState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := GooseState{StreamID: req.StreamID, CatalogRevision: a.Catalog.Revision, Values: []SignalValue{}}
	if err := a.ready(); err != nil {
		return out, err
	}
	s, err := a.stream(req.StreamID, "goose", false)
	if err != nil {
		return out, err
	}
	var data []goose.DataValue
	if strings.HasPrefix(s.ID, "goose_pub/") {
		out.Direction = "output"
		v, status, err := a.agent.GooseOutput(s.Name)
		out.Module = status
		if err != nil {
			return out, err
		}
		out.Available, out.Fresh = true, status.State == control.StatusRunning
		out.ChangedAt, out.Test, out.Simulation = &v.ChangedAt, v.Test, v.Simulation
		out.SchemaMatched = len(s.Signals) == len(v.Data)
		out.Warning = "Configured output, not confirmation of delivery to the terminal."
		data = v.Data
	} else {
		out.Direction = "input"
		values, status, err := a.agent.GooseInput(s.Name)
		out.Module = status
		if err != nil {
			return out, err
		}
		if len(values) == 0 {
			out.Warning = "No accepted GOOSE data yet."
			return out, nil
		}
		if len(values) != 1 {
			return out, errors.New("multiple live streams match this subscription; configure an exact stream filter")
		}
		v := values[0]
		out.Available, out.Fresh = true, status.State == control.StatusRunning && !v.Stale
		out.ReceivedAt = &v.ReceivedAt
		out.Test, out.Simulation = v.PDU.Test, v.PDU.Simulation
		data = v.PDU.AllData
		if desc := a.descriptions[s.ID]; desc != nil {
			if err := desc.ValidatePDU(v.PDU); err == nil {
				out.SchemaMatched = true
			} else {
				out.Warning = "ICD mismatch: " + err.Error()
			}
		} else {
			out.Warning = "No matching ICD signal schema; entries are positional only."
		}
	}
	for i, v := range data {
		var schema *catalog.Signal
		if out.SchemaMatched && i < len(s.Signals) {
			schema = &s.Signals[i]
		}
		out.Values = append(out.Values, signalValue(v, fmt.Sprintf("Entry%d", i+1), schema))
	}
	return out, nil
}

func signalValue(v goose.DataValue, name string, schema *catalog.Signal) SignalValue {
	out := SignalValue{Name: name, Type: v.Type.String()}
	if schema != nil {
		out.ID, out.Name, out.Type = schema.ID, schema.Name, schema.Type
	}
	switch v.Type {
	case goose.DataTypeBoolean:
		out.Value = v.Bool
	case goose.DataTypeInteger, goose.DataTypeBCD:
		out.Value = v.Int
	case goose.DataTypeUnsigned:
		out.Value = v.UInt
	case goose.DataTypeFloatingPoint, goose.DataTypeReal:
		out.Value = finite(v.Float)
	case goose.DataTypeVisibleString:
		out.Value = v.String
	case goose.DataTypeUTCTime, goose.DataTypeBinaryTime:
		out.Value = v.Time.UTC().Format(time.RFC3339Nano)
	case goose.DataTypeBitString, goose.DataTypeBooleanArray, goose.DataTypeOctetString:
		out.Value = map[string]any{"hex": hex.EncodeToString(v.Bytes), "bit_length": v.BitLength}
		if out.Type == "Quality" {
			var mask uint32
			for bit := 0; bit < v.BitLength && bit < 32 && bit/8 < len(v.Bytes); bit++ {
				if v.Bytes[bit/8]&(1<<(7-bit%8)) != 0 {
					mask |= 1 << bit
				}
			}
			out.Value = mask
		}
	case goose.DataTypeStructure, goose.DataTypeArray:
		for i, child := range v.Children {
			var childSchema *catalog.Signal
			if schema != nil && v.Type == goose.DataTypeStructure && i < len(schema.Children) {
				childSchema = &schema.Children[i]
			}
			// Array schemas are templates, never concrete signal addresses.
			out.Children = append(out.Children, signalValue(child, fmt.Sprintf("[%d]", i), childSchema))
		}
	}
	return out
}

func (a *API) GooseSet(req GooseSetRequest) (Applied, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Applied{CatalogRevision: a.Catalog.Revision, Note: "Applied to publisher state; does not confirm terminal reception."}
	if err := a.ready(); err != nil {
		return out, err
	}
	if err := a.streamIdle(req.StreamID); err != nil {
		return out, err
	}
	s, err := a.stream(req.StreamID, "goose", true)
	if err != nil {
		return out, err
	}
	current, status, err := a.agent.GooseOutput(s.Name)
	if err != nil {
		return out, err
	}
	if status.State != control.StatusRunning {
		return out, errors.New("GOOSE publisher is not running")
	}
	current, err = a.prepareGoose(req, s, current)
	if err != nil {
		return out, err
	}
	out.Changed, err = a.agent.ApplyGoose(s.Name, current.Data, current.Test, current.Simulation)
	out.Applied = err == nil
	return out, err
}

// prepareGoose validates without writes and owns the returned data slice.
// Sequence preflight uses the preceding result to validate cumulative changes.
func (a *API) prepareGoose(req GooseSetRequest, s catalog.Stream, current goosepub.Snapshot) (goosepub.Snapshot, error) {
	out := current
	if req.Revision != a.Catalog.Revision {
		return out, errors.New("catalog_revision mismatch; call get_catalog")
	}
	if len(req.Changes) == 0 || len(req.Changes) > len(s.Signals) {
		return out, errors.New("changes must contain distinct configured signals")
	}
	if len(current.Data) != len(s.Signals) {
		return out, errors.New("publisher schema differs from catalog")
	}
	current.Data = goose.CloneDataValues(current.Data)
	seen := map[string]bool{}
	for _, change := range req.Changes {
		if seen[change.SignalID] {
			return out, errors.New("duplicate signal_id")
		}
		seen[change.SignalID] = true
		index := -1
		for i, sig := range s.Signals {
			if sig.ID == change.SignalID {
				index = i
				break
			}
		}
		if index < 0 {
			return out, errors.New("signal_id is not a writable entry of this stream")
		}
		value, err := decodeGooseValue(s.Signals[index].Type, change.Value)
		if err != nil {
			return out, fmt.Errorf("%s: %w", change.SignalID, err)
		}
		current.Data[index] = value
	}
	header := a.gooseHeaders[s.Name]
	header.AllData, header.NumDatSetEntries = current.Data, uint32(len(current.Data))
	if len(goose.Encode(&header))-14 > 1500 {
		return out, errors.New("GOOSE data exceeds standard Ethernet MTU")
	}
	return current, nil
}

func decodeGooseValue(kind string, raw json.RawMessage) (goose.DataValue, error) {
	var out goose.DataValue
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return out, errors.New("value is required and must not be null")
	}
	var err error
	switch kind {
	case "BOOLEAN":
		out.Type = goose.DataTypeBoolean
		err = json.Unmarshal(raw, &out.Bool)
	case "INTEGER":
		out.Type = goose.DataTypeInteger
		err = json.Unmarshal(raw, &out.Int)
	case "UNSIGNED":
		out.Type = goose.DataTypeUnsigned
		err = json.Unmarshal(raw, &out.UInt)
	case "FLOAT":
		out.Type = goose.DataTypeFloatingPoint
		err = json.Unmarshal(raw, &out.Float)
		if math.IsNaN(out.Float) || math.IsInf(out.Float, 0) || math.Abs(out.Float) > math.MaxFloat32 {
			return out, errors.New("float must fit a finite float32")
		}
	case "VISIBLE_STRING":
		out.Type = goose.DataTypeVisibleString
		err = json.Unmarshal(raw, &out.String)
	case "UTC_TIME":
		var value string
		err = json.Unmarshal(raw, &value)
		if err == nil {
			out.Time, err = time.Parse(time.RFC3339Nano, value)
		}
		out.Type = goose.DataTypeUTCTime
		if err == nil && (out.Time.Unix() < 0 || out.Time.Unix() > math.MaxUint32) {
			return out, errors.New("UTC time is outside the protocol range")
		}
	case "Quality":
		var mask uint32
		err = json.Unmarshal(raw, &mask)
		if mask >= 1<<goose.QualityBitLength {
			return out, errors.New("quality mask exceeds supported bits")
		}
		out = goose.NewQualityBitString(mask)
	default:
		return out, errors.New("unsupported writable signal type")
	}
	if err != nil {
		return out, fmt.Errorf("value does not match %s", kind)
	}
	return out, nil
}

func (a *API) SVGet(req StreamRequest) (SVState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := SVState{StreamID: req.StreamID, CatalogRevision: a.Catalog.Revision, Channels: []ChannelValue{}}
	if err := a.ready(); err != nil {
		return out, err
	}
	s, err := a.stream(req.StreamID, "sv", false)
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(s.ID, "sv_pub/") {
		out.Direction, out.Basis, out.PhaseReference = "output", "secondary", "electrical phase of the configured generator"
		v, status, err := a.agent.SVOutput(s.Name)
		out.Module = status
		if err != nil {
			return out, err
		}
		out.Available, out.Fresh = v.Ready, status.State == control.StatusRunning && v.Ready
		out.Warning = "Configured output, not confirmation of delivery to the terminal."
		if !v.Ready {
			out.Warning = "Waveform has not been applied yet."
			return out, nil
		}
		for i, setting := range v.Settings {
			out.Channels = append(out.Channels, ChannelValue{ID: s.Signals[i].ID, Name: s.Signals[i].Name, Unit: s.Signals[i].Unit,
				RMS: finite(setting.RMS), Phase: finite(setting.PhaseDeg), Frequency: finite(setting.Frequency), Quality: uint32(setting.Quality)})
		}
	} else {
		out.Direction, out.Basis = "input", "wire_scaled; primary_or_secondary_unspecified"
		out.PhaseReference, _ = s.Metadata["base_vector"].(string)
		values, status, err := a.agent.SVInput(s.Name)
		out.Module = status
		if err != nil {
			return out, err
		}
		if len(values) == 0 {
			out.Warning = "No accepted SV data yet."
			return out, nil
		}
		if len(values) != 1 {
			return out, errors.New("multiple live streams match this subscription; configure an exact stream filter")
		}
		v := values[0]
		out.ReceivedAt = &v.ReceivedAt
		if v.Stats == nil {
			out.Warning = "Waiting for the first measurement window."
			return out, nil
		}
		out.Available, out.Fresh = true, status.State == control.StatusRunning && !v.Stale
		out.MeasuredAt = &v.MeasuredAt
		for i, sig := range s.Signals {
			ch := ChannelValue{ID: sig.ID, Name: sig.Name, Unit: sig.Unit, Instantaneous: finite(v.Stats.Instant[i]), Quality: uint32(v.Stats.Quality[i])}
			if v.RMSReady {
				ch.RMS = finite(v.Stats.RMS[i])
			}
			ch.Phase = finite(v.Stats.Angle[i])
			if v.Stats.FrequencyHz[i] > 0 {
				ch.Frequency = finite(v.Stats.FrequencyHz[i])
			}
			out.Channels = append(out.Channels, ch)
		}
		out.Warning = "Null RMS/phase/frequency means the measurement is not available; input values do not include KI/KU display multipliers."
	}
	return out, nil
}

func (a *API) SVSet(req SVSetRequest) (Applied, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Applied{CatalogRevision: a.Catalog.Revision, Note: "Applied to generator settings; frequency, quality, simulation and synchronization are preserved."}
	if err := a.ready(); err != nil {
		return out, err
	}
	if err := a.streamIdle(req.StreamID); err != nil {
		return out, err
	}
	s, err := a.stream(req.StreamID, "sv", true)
	if err != nil {
		return out, err
	}
	current, status, err := a.agent.SVOutput(s.Name)
	if err != nil {
		return out, err
	}
	if status.State != control.StatusRunning {
		return out, errors.New("SV publisher is not running")
	}
	next, err := a.prepareSV(req, s, current)
	if err != nil {
		return out, err
	}
	out.Changed = next.Settings != current.Settings
	err = a.agent.ApplySV(s.Name, next.Settings, next.Simulation, next.Synch)
	out.Applied = err == nil
	return out, err
}

func (a *API) prepareSV(req SVSetRequest, s catalog.Stream, current svpub.Snapshot) (svpub.Snapshot, error) {
	out := current
	if req.Revision != a.Catalog.Revision {
		return out, errors.New("catalog_revision mismatch; call get_catalog")
	}
	if len(req.Changes) == 0 || len(req.Changes) > sv.NumChannels {
		return out, errors.New("changes must contain 1 to 8 distinct channels")
	}
	if len(s.Signals) != sv.NumChannels {
		return out, errors.New("publisher schema differs from catalog")
	}
	if !current.Ready {
		return out, errors.New("engineer must apply the initial SV waveform in Manual before starting MCP")
	}
	seen := map[string]bool{}
	for _, change := range req.Changes {
		if seen[change.SignalID] {
			return out, errors.New("duplicate signal_id")
		}
		seen[change.SignalID] = true
		index := -1
		for i, sig := range s.Signals {
			if sig.ID == change.SignalID {
				index = i
				break
			}
		}
		if index < 0 {
			return out, errors.New("signal_id is not a channel of this output stream")
		}
		if change.RMS == nil && change.Phase == nil {
			return out, errors.New("supply rms and/or phase_deg")
		}
		if change.RMS != nil {
			if finite(*change.RMS) == nil || *change.RMS < 0 {
				return out, errors.New("RMS must be finite and non-negative")
			}
			scale := sv.CurrentScale
			if index >= sv.ChUa {
				scale = sv.VoltageScale
			}
			if *change.RMS*math.Sqrt2*scale > math.MaxInt32 {
				return out, errors.New("RMS exceeds the SV raw int32 range")
			}
			current.Settings[index].RMS = *change.RMS
		}
		if change.Phase != nil {
			if finite(*change.Phase) == nil {
				return out, errors.New("phase must be finite")
			}
			phase := math.Mod(*change.Phase, 360)
			current.Settings[index].PhaseDeg = phase
		}
	}
	return current, nil
}

func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// Decode rejects unknown fields so misspelled commands cannot silently do less
// than requested. Raw GOOSE values preserve integer precision during decoding.
func Decode(raw []byte, value any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("arguments must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("invalid arguments or unknown fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("arguments must contain one JSON object")
	}
	return nil
}
