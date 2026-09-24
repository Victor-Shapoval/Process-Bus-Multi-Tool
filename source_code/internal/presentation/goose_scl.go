package presentation

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
	"gopkg.in/yaml.v3"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
)

func projectICDFiles(root string) ([]string, error) { return sclmodel.ProjectICDFiles(root) }

func projectGooseDescriptions(root string) ([]sclmodel.GooseStream, string) {
	return sclmodel.ProjectGooseDescriptions(root)
}

func gooseDescription(sub config.GooseSubscriber, streams []sclmodel.GooseStream) (*sclmodel.GooseStream, string) {
	return sclmodel.GooseDescription(sub, streams)
}

func describedGooseEntries(g *sclmodel.GooseStream, pdu *goose.PDU) ([]gooseDatasetDisplayEntry, string) {
	if pdu != nil {
		if err := g.ValidatePDU(pdu); err != nil {
			return gooseDatasetDisplayEntries(pdu.AllData), "ICD mismatch: " + err.Error() + ". Using Entry labels."
		}
	}
	entries := make([]gooseDatasetDisplayEntry, len(g.Entries))
	for i, t := range g.Entries {
		var v *goose.DataValue
		if pdu != nil {
			v = &pdu.AllData[i]
		}
		node := describedGooseNode(t, t.Name, v)
		entries[i] = gooseDatasetDisplayEntry{Name: t.Name, Summary: node.Value, Value: node}
	}
	if pdu == nil {
		return entries, "ICD description loaded. Waiting for a matching frame."
	}
	return entries, "ICD DataSet matches received frame."
}

func describedGooseNode(t *sclmodel.Type, name string, v *goose.DataValue) gooseDatasetDisplayNode {
	n := gooseDatasetDisplayNode{Name: name, Type: t.Kind, Value: "—"}
	if v != nil {
		n.Value = gooseDataValueText(*v)
	}
	if t.Kind == "STRUCTURE" {
		for i, c := range t.Children {
			var value *goose.DataValue
			if v != nil {
				value = &v.Children[i]
			}
			n.Children = append(n.Children, describedGooseNode(c, c.Name, value))
		}
	} else if t.Kind == "ARRAY" && len(t.Children) == 1 {
		for i := 0; i < t.Size; i++ {
			var value *goose.DataValue
			if v != nil {
				value = &v.Children[i]
			}
			n.Children = append(n.Children, describedGooseNode(t.Children[0], fmt.Sprintf("[%d]", i), value))
		}
	}
	return n
}

func (s *uiState) showGooseImportDialog(target *yaml.Node, refresh func()) {
	open := dialog.NewFileOpen(func(r fyne.URIReadCloser, err error) {
		if err != nil {
			showError(err, s.window)
			return
		}
		if r == nil {
			return
		}
		defer r.Close()
		raw, err := io.ReadAll(io.LimitReader(r, (32<<20)+1))
		if err != nil {
			showError(err, s.window)
			return
		}
		streams, err := sclmodel.ParseGooseSCL(raw)
		if err != nil {
			showError(err, s.window)
			return
		}
		if len(streams) == 0 {
			showError(fmt.Errorf("no GOOSE publications found in SCL"), s.window)
			return
		}
		options := make([]string, len(streams))
		for i, g := range streams {
			options[i] = fmt.Sprintf("%d. %s", i+1, g.GocbRef)
		}
		selected := 0
		info := widget.NewLabel("")
		info.Wrapping = fyne.TextWrapWord
		choice := widget.NewSelect(options, func(v string) {
			for i, o := range options {
				if o == v {
					selected = i
					break
				}
			}
			g := streams[selected]
			info.SetText(fmt.Sprintf("MAC: %s   APPID: 0x%04X\nDataSet: %s\nGoID: %s   confRev: %d\n%s\nSource MAC is not supplied by SCL. Reception policy flags are retained.\nFor Manual signal names, keep this ICD in the project root.", g.DstMAC, g.AppID, g.DatSet, g.GoID, g.ConfRev, g.FrameError))
		})
		choice.SetSelectedIndex(0)
		confirm := dialog.NewCustomConfirm("Import GOOSE subscription", "Apply", "Cancel", container.NewVBox(choice, info), func(ok bool) {
			if !ok {
				return
			}
			if err := applyGooseImport(target, streams[selected]); err != nil {
				showError(err, s.window)
				return
			}
			refresh()
			slog.Info("GOOSE subscription imported into editor", "file", r.URI().Name(), "gocb_ref", streams[selected].GocbRef)
			s.statusBar.SetText("GOOSE parameters imported. Review and Save to apply.")
		}, s.window)
		confirm.Resize(fyne.NewSize(720, 350))
		confirm.Show()
	}, s.window)
	open.SetFilter(storage.NewExtensionFileFilter([]string{".icd", ".cid", ".scd", ".iid"}))
	if uri, err := storage.ListerForURI(storage.NewFileURI(s.configPath)); err == nil {
		open.SetLocation(uri)
	}
	open.Show()
}

// Only frame identity fields change. No signal schema is stored in YAML.
func applyGooseImport(target *yaml.Node, g sclmodel.GooseStream) error {
	if g.FrameError != "" {
		return fmt.Errorf("cannot import stream: %s", g.FrameError)
	}
	for key, value := range map[string]string{"name": g.Name, "dst_mac": g.DstMAC, "src_mac": "", "gocb_ref": g.GocbRef, "dat_set": g.DatSet, "go_id": g.GoID} {
		setMappingValue(target, key, scalarString(value))
	}
	setMappingValue(target, "app_id", scalarInt(fmt.Sprintf("0x%04X", g.AppID)))
	setMappingValue(target, "conf_rev", scalarInt(strconv.FormatUint(uint64(g.ConfRev), 10)))
	setMappingValue(target, "match_any_app_id", scalarBool(false))
	for key, value := range map[string]interface{}{"vlan_id": g.VLANID, "vlan_pri": g.VLANPriority} {
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
		switch v := value.(type) {
		case *uint16:
			if v != nil {
				n = scalarInt(strconv.Itoa(int(*v)))
			}
		case *uint8:
			if v != nil {
				n = scalarInt(strconv.Itoa(int(*v)))
			}
		}
		setMappingValue(target, key, n)
	}
	return nil
}
