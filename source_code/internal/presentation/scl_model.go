package presentation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/sclmodel"
)

type sclModelView struct {
	owner                     *uiState
	model                     *sclmodel.Model
	data                      *sclmodel.Tree
	tree                      *widget.Tree
	ip, port                  *widget.Entry
	status, detail            *widget.Label
	read, connect, save       *widget.Button
	readings                  map[string]sclmodel.Reading
	rows                      map[*fyne.Container]string
	cancel                    context.CancelFunc
	epoch                     uint64
	disposed, connected, busy bool
	fromICD                   bool
	selected                  string
}

func (s *uiState) sclModelPage() fyne.CanvasObject {
	p := &sclModelView{owner: s, readings: map[string]sclmodel.Reading{}, rows: map[*fyne.Container]string{}}
	s.sclModelView = p
	p.ip = widget.NewEntry()
	p.ip.SetPlaceHolder("192.168.1.100")
	p.port = widget.NewEntry()
	p.port.SetText("102")
	p.status = widget.NewLabel("Read a device model to begin.")
	p.status.Wrapping = fyne.TextWrapWord
	p.detail = widget.NewLabel("Object / MMS type [FC] / Value")
	p.detail.Wrapping = fyne.TextWrapWord
	p.read = widget.NewButton("Read Model", p.readModel)
	p.connect = widget.NewButton("Connect", p.toggleConnect)
	p.save = widget.NewButton("Save ICD", p.saveICD)
	p.tree = widget.NewTree(func(id string) []string {
		if p.data != nil && p.data.Nodes[id] != nil {
			return p.data.Nodes[id].Children
		}
		return nil
	}, func(id string) bool {
		return p.data != nil && p.data.Nodes[id] != nil && len(p.data.Nodes[id].Children) > 0
	}, func(bool) fyne.CanvasObject {
		name := widget.NewLabel("")
		name.Truncation = fyne.TextTruncateEllipsis
		kind := widget.NewLabel("")
		kind.Truncation = fyne.TextTruncateEllipsis
		value := widget.NewLabel("")
		value.Truncation = fyne.TextTruncateEllipsis
		return container.NewGridWithColumns(3, name, kind, value)
	}, func(id string, _ bool, obj fyne.CanvasObject) {
		row := obj.(*fyne.Container)
		p.rows[row] = id
		if p.data == nil {
			return
		}
		n := p.data.Nodes[id]
		if n == nil {
			return
		}
		row.Objects[0].(*widget.Label).SetText(n.Name)
		row.Objects[1].(*widget.Label).SetText(n.Type)
		row.Objects[2].(*widget.Label).SetText(p.valueText(id))
	})
	p.tree.OnSelected = func(id string) { p.selected = id; p.updateDetail() }
	p.ip.OnChanged = func(string) { p.endpointChanged() }
	p.port.OnChanged = func(string) { p.endpointChanged() }
	left := container.NewVBox(widget.NewForm(widget.NewFormItem("IP", p.ip), widget.NewFormItem("Port", p.port)), p.read, p.connect, p.save, widget.NewSeparator(), p.status)
	right := container.NewBorder(nil, p.detail, nil, nil, p.tree)
	split := container.NewHSplit(container.NewVScroll(left), right)
	split.Offset = 0.23
	// Restoring an ICD never opens a network connection.
	names, err := projectICDFiles(s.configPath)
	if err != nil {
		p.status.SetText("Cannot list project ICD files: " + err.Error())
		slog.Error("project ICD enumeration failed", "path", s.configPath, "error", err)
	} else if len(names) == 1 {
		p.loadICD(names[0])
	} else if len(names) > 1 {
		slog.Warn("project ICD is ambiguous", "path", s.configPath, "files", names)
		p.status.SetText("Multiple ICD files found. Keep one terminal ICD in the project root and reopen the project.")
	}
	p.buttons()
	return container.NewPadded(split)
}

func (p *sclModelView) endpoint() (sclmodel.Endpoint, error) {
	port, err := strconv.Atoi(strings.TrimSpace(p.port.Text))
	if err != nil {
		return sclmodel.Endpoint{}, fmt.Errorf("enter a numeric port")
	}
	e := sclmodel.Endpoint{IP: strings.TrimSpace(p.ip.Text), Port: port}
	return e, e.Validate()
}
func (p *sclModelView) matches() bool {
	e, err := p.endpoint()
	return err == nil && p.model != nil && p.model.Endpoint.IP == e.IP && (p.fromICD || p.model.Endpoint.Port == e.Port)
}
func (p *sclModelView) buttons() {
	p.read.Enable()
	p.ip.Enable()
	p.port.Enable()
	p.connect.Disable()
	p.save.Disable()
	p.read.SetText("Read Model")
	p.connect.SetText("Connect")
	if p.model != nil {
		p.save.Enable()
	}
	if p.matches() && p.owner.deps.DialModel != nil {
		p.connect.Enable()
	}
	if p.busy {
		p.ip.Disable()
		p.port.Disable()
		p.save.Disable()
		p.connect.Disable()
		if p.connected {
			p.save.Enable()
			p.read.Disable()
			p.connect.Enable()
			p.connect.SetText("Disconnect")
		} else {
			p.read.SetText("Cancel")
		}
	}
	if p.owner.deps.DialModel == nil {
		p.read.Disable()
		p.status.SetText("MMS client is unavailable.")
	}
}
func (p *sclModelView) endpointChanged() {
	if p.model != nil && !p.matches() {
		p.status.SetText("Tree belongs to " + p.model.Endpoint.String() + ". Read Model for the new address.")
	}
	p.buttons()
}
func (p *sclModelView) dispose() {
	if p.disposed {
		return
	}
	if p.busy {
		slog.Info("SCL operation canceled", "reason", "view_closed")
	}
	p.disposed = true
	p.epoch++
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *sclModelView) begin() (context.Context, uint64) {
	p.epoch++
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.busy = true
	p.buttons()
	return ctx, p.epoch
}
func (p *sclModelView) finish(epoch uint64, err error) bool {
	if p.disposed || epoch != p.epoch {
		return false
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.cancel = nil
	p.busy = false
	p.connected = false
	p.buttons()
	if err != nil && err != context.Canceled {
		p.status.SetText(err.Error())
		slog.Error("SCL Model operation failed", "ip", p.ip.Text, "port", p.port.Text, "error", err)
	} else if err == context.Canceled {
		slog.Info("SCL operation canceled", "reason", "user_request")
	}
	p.tree.Refresh()
	return true
}

func (p *sclModelView) readModel() {
	if p.busy {
		if p.cancel != nil {
			p.cancel()
			p.status.SetText("Canceling…")
			p.read.Disable()
		}
		return
	}
	endpoint, err := p.endpoint()
	if err != nil {
		showError(err, p.owner.window)
		return
	}
	ctx, epoch := p.begin()
	p.status.SetText("Connecting to " + endpoint.String() + "…")
	slog.Info("SCL model read requested", "endpoint", endpoint.String())
	dial := p.owner.deps.DialModel
	go func() {
		model, data, err := func() (*sclmodel.Model, *sclmodel.Tree, error) {
			client, err := dial(ctx, endpoint)
			if err != nil {
				return nil, nil, err
			}
			defer client.Close()
			model, err := client.Discover(ctx, func(message string) {
				fyne.Do(func() {
					if !p.disposed && p.epoch == epoch && ctx.Err() == nil {
						p.status.SetText(message)
					}
				})
			})
			if err != nil {
				return nil, nil, err
			}
			data, err := sclmodel.BuildTree(model)
			return model, data, err
		}()
		fyne.Do(func() {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if !p.finish(epoch, err) {
				return
			}
			if err != nil {
				if err == context.Canceled {
					p.status.SetText("Read canceled. Previous model retained.")
				}
				return
			}
			p.model = model
			p.fromICD = false
			p.data = data
			p.readings = map[string]sclmodel.Reading{}
			p.rows = map[*fyne.Container]string{}
			p.selected = ""
			p.detail.SetText("Object / MMS type [FC] / Value")
			p.tree.CloseAllBranches()
			p.tree.Refresh()
			p.tree.ScrollToTop()
			p.buttons()
			p.status.SetText(fmt.Sprintf("Model read: %d logical devices, %d tree nodes.\nDisconnected.\n%d discovery warnings.\nSave ICD in the project directory to restore it next time.", len(model.Devices), len(data.Nodes)-1, len(model.Warnings)))
			slog.Info("SCL model read", "endpoint", endpoint.String(), "nodes", len(data.Nodes)-1, "warnings", model.Warnings)
		})
	}()
}

func (p *sclModelView) toggleConnect() {
	if p.busy {
		if p.cancel != nil {
			p.cancel()
			p.connect.Disable()
			p.status.SetText("Disconnecting…")
		}
		return
	}
	if !p.matches() {
		return
	}
	endpoint, _ := p.endpoint()
	dial := p.owner.deps.DialModel
	ctx, epoch := p.begin()
	p.status.SetText("Connecting to " + endpoint.String() + "…")
	slog.Info("SCL monitoring requested", "endpoint", endpoint.String())
	go func() {
		err := func() error {
			client, err := dial(ctx, endpoint)
			if err != nil {
				return err
			}
			defer client.Close()
			fyne.Do(func() {
				if !p.disposed && p.epoch == epoch && ctx.Err() == nil {
					p.connected = true
					slog.Info("SCL monitoring connected", "endpoint", endpoint.String())
					p.buttons()
					p.status.SetText("Connected. Updating visible attributes every 1 s.")
				}
			})
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				requested := make(chan []sclmodel.Target, 1)
				fyne.Do(func() {
					if !p.disposed && p.epoch == epoch && ctx.Err() == nil {
						requested <- p.visibleTargets()
					} else {
						requested <- nil
					}
				})
				var targets []sclmodel.Target
				select {
				case <-ctx.Done():
					return ctx.Err()
				case targets = <-requested:
				}
				readings, err := client.Read(ctx, targets)
				fyne.Do(func() {
					if !p.disposed && p.epoch == epoch && ctx.Err() == nil {
						for id, r := range readings {
							if r.Error != "" {
								old := p.readings[id]
								if old.Error != r.Error {
									slog.Warn("SCL attribute read failed", "endpoint", endpoint.String(), "attribute", id, "error", r.Error)
								}
								old.Error = r.Error
								p.readings[id] = old
							} else {
								if p.readings[id].Error != "" {
									slog.Info("SCL attribute read restored", "endpoint", endpoint.String(), "attribute", id)
								}
								p.readings[id] = r
							}
						}
						p.tree.Refresh()
						p.updateDetail()
					}
				})
				if err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}()
		fyne.Do(func() {
			if !p.finish(epoch, err) {
				return
			}
			if err == nil || err == context.Canceled {
				p.status.SetText("Disconnected. Last values are stale.")
			}
			p.updateDetail()
			slog.Info("SCL monitoring stopped", "endpoint", endpoint.String())
		})
	}()
}

func (p *sclModelView) visibleTargets() []sclmodel.Target {
	if p.data == nil || !p.tree.Visible() {
		return nil
	}
	if tabs := p.owner.mainTabs; tabs != nil && (tabs.Selected() == nil || tabs.Selected().Text != "Configuration") {
		return nil
	}
	if tabs := p.owner.configurationTabs; tabs != nil && (tabs.Selected() == nil || tabs.Selected().Text != "SCL Model") {
		return nil
	}
	driver := p.owner.app.Driver()
	origin := driver.AbsolutePositionForObject(p.tree)
	bottom := origin.Y + p.tree.Size().Height
	var result []sclmodel.Target
	seen := map[string]bool{}
	for row, id := range p.rows {
		n := p.data.Nodes[id]
		if n == nil || n.Target == nil || seen[id] || !row.Visible() || !p.data.Expanded(id, p.tree.IsBranchOpen) {
			continue
		}
		pos := driver.AbsolutePositionForObject(row)
		if pos.Y+row.Size().Height <= origin.Y || pos.Y >= bottom {
			continue
		}
		result = append(result, *n.Target)
		seen[id] = true
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func (p *sclModelView) valueText(id string) string {
	if p.data != nil {
		if n := p.data.Nodes[id]; n != nil && n.Target == nil {
			return n.Snapshot
		}
	}
	r, ok := p.readings[id]
	if !ok {
		return ""
	}
	if r.Error != "" {
		if r.Value != "" {
			return r.Value + " (stale; " + r.Error + ")"
		}
		return r.Error
	}
	if !p.connected || time.Since(r.ReadAt) > 3*time.Second {
		return r.Value + " (stale)"
	}
	return r.Value
}
func (p *sclModelView) updateDetail() {
	if p.data == nil || p.selected == "" {
		return
	}
	n := p.data.Nodes[p.selected]
	if n == nil {
		return
	}
	text := n.Name + " — " + n.Type
	if n.Snapshot != "" {
		text += "\n" + n.Snapshot
	}
	if n.Target != nil {
		text = n.Target.Domain + "/" + n.Target.Item + "\n" + p.valueText(n.ID)
		if r := p.readings[n.ID]; !r.ReadAt.IsZero() {
			text += "\nLast successful read: " + r.ReadAt.Format("15:04:05.000")
		}
	}
	p.detail.SetText(text)
}

func (p *sclModelView) saveICD() {
	if p.owner.mcpActive() {
		showError(fmt.Errorf("stop MCP before saving the project ICD"), p.owner.window)
		return
	}
	if p.model == nil {
		return
	}
	data, warnings, err := sclmodel.ExportICD(p.model)
	if err != nil {
		showError(err, p.owner.window)
		return
	}
	fileName := sclModelFileName(p.model.Endpoint.IP)
	showSave := func() {
		save := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
			if err != nil {
				showError(err, p.owner.window)
				return
			}
			if w == nil {
				return
			}
			name := w.URI().String()
			_, writeErr := w.Write(data)
			closeErr := w.Close()
			if writeErr != nil {
				showError(writeErr, p.owner.window)
				return
			}
			if closeErr != nil {
				showError(closeErr, p.owner.window)
				return
			}
			p.status.SetText("ICD saved.")
			p.buttons()
			if !p.disposed && p.owner.catalogView != nil {
				p.owner.catalogView.refresh(true)
			}
			slog.Info("discovered ICD exported", "path", name, "warnings", warnings)
		}, p.owner.window)
		save.SetFileName(fileName)
		save.SetFilter(storage.NewExtensionFileFilter([]string{".icd"}))
		if uri, err := storage.ListerForURI(storage.NewFileURI(p.owner.configPath)); err == nil {
			save.SetLocation(uri)
		}
		save.Show()
	}
	if len(warnings) > 0 {
		text := strings.Join(warnings, "\n\n")
		if len(text) > 16000 {
			text = text[:16000] + "\n… Additional warnings truncated. Unsupported objects are omitted from the ICD."
		}
		label := widget.NewLabel(text)
		label.Wrapping = fyne.TextWrapWord
		confirm := dialog.NewCustomConfirm("Reconstructed ICD", "Save ICD", "Cancel", container.NewVScroll(label), func(ok bool) {
			if ok {
				showSave()
			}
		}, p.owner.window)
		confirm.Resize(fyne.NewSize(680, 420))
		confirm.Show()
	} else {
		showSave()
	}
}

func sclModelFileName(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return "DiscoveredIED.icd"
	}
	// Colons cannot be used in filenames on every supported filesystem.
	return strings.ReplaceAll(parsed.String(), ":", "_") + ".icd"
}

func (p *sclModelView) loadICD(name string) {
	if p.busy || p.disposed || name == "" {
		return
	}
	model, data, err := readProjectICD(filepath.Join(p.owner.configPath, name))
	if err != nil {
		p.status.SetText("Cannot load " + name + ": " + err.Error() + ". Previous model retained.")
		slog.Warn("project ICD load failed", "file", name, "error", err)
		return
	}
	p.model, p.data, p.fromICD = model, data, true
	p.readings = map[string]sclmodel.Reading{}
	p.rows = map[*fyne.Container]string{}
	p.selected = ""
	p.detail.SetText("Object / MMS type [FC] / Value")
	p.ip.SetText(model.Endpoint.IP)
	p.port.SetText(strconv.Itoa(model.Endpoint.Port))
	p.tree.CloseAllBranches()
	p.tree.Refresh()
	p.tree.ScrollToTop()
	p.updateDetail()
	p.status.SetText("ICD loaded: " + name + "\nDisconnected. Values appear after Connect.\nMMS port defaults to 102; change it if needed.")
	p.buttons()
	slog.Info("project ICD loaded", "file", name, "nodes", len(data.Nodes)-1)
}

func readProjectICD(path string) (*sclmodel.Model, *sclmodel.Tree, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	model, err := sclmodel.ImportICD(raw)
	if err != nil {
		return nil, nil, err
	}
	if model.Endpoint.IP == "" {
		ip := net.ParseIP(strings.ReplaceAll(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), "_", ":"))
		if ip == nil {
			return nil, nil, fmt.Errorf("ICD has no device IP address")
		}
		model.Endpoint.IP = ip.String()
	}
	data, err := sclmodel.BuildTree(model)
	return model, data, err
}
