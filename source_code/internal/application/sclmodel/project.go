package sclmodel

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"pbmt/internal/config"
	"strings"
)

func ProjectICDFiles(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.EqualFold(filepath.Ext(e.Name()), ".icd") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func ProjectGooseDescriptions(root string) ([]GooseStream, string) {
	names, err := ProjectICDFiles(root)
	if err != nil {
		return nil, "ICD unavailable: " + err.Error()
	}
	if len(names) == 0 {
		return nil, "No project ICD. Signals are displayed as Entry1, Entry2…"
	}
	if len(names) != 1 {
		return nil, "Multiple project ICD files. Keep one ICD in the project root; using Entry labels."
	}
	f, err := os.Open(filepath.Join(root, names[0]))
	if err != nil {
		return nil, "ICD unavailable: " + err.Error()
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, "ICD read failed: " + err.Error()
	}
	streams, err := ParseGooseSCL(raw)
	if err != nil {
		return nil, "ICD parse failed: " + err.Error()
	}
	return streams, "Signal descriptions: " + names[0] + ". Values come from received frames."
}

func GooseDescription(sub config.GooseSubscriber, streams []GooseStream) (*GooseStream, string) {
	var matches []GooseStream
	for _, g := range streams {
		if sub.GocbRef != "" && sub.GocbRef == g.GocbRef && (sub.DatSet == "" || sub.DatSet == g.DatSet) {
			matches = append(matches, g)
		}
	}
	if len(matches) != 1 {
		return nil, "No unique ICD description for this subscription; using Entry labels."
	}
	g := matches[0]
	if g.SchemaError != "" {
		return nil, "ICD DataSet unavailable: " + g.SchemaError
	}
	if g.FrameError != "" {
		return nil, "ICD stream unavailable: " + g.FrameError
	}
	if !sub.MatchAnyAppID && uint16(sub.AppID) != g.AppID {
		return nil, "Configured APPID differs from ICD; using Entry labels."
	}
	if sub.ConfRev != nil && *sub.ConfRev != g.ConfRev {
		return nil, "Configured confRev differs from ICD; using Entry labels."
	}
	if mac, err := net.ParseMAC(sub.DstMAC); sub.DstMAC != "" && (err != nil || mac.String() != g.DstMAC) {
		return nil, "Configured destination MAC differs from ICD; using Entry labels."
	}
	if sub.GoID != "" && sub.GoID != g.GoID {
		return nil, "Configured GoID differs from ICD; using Entry labels."
	}
	if sub.VLANID != nil && g.VLANID != nil && *sub.VLANID != *g.VLANID {
		return nil, "Configured VLAN differs from ICD; using Entry labels."
	}
	if sub.VLANPri != nil && g.VLANPriority != nil && *sub.VLANPri != *g.VLANPriority {
		return nil, "Configured VLAN priority differs from ICD; using Entry labels."
	}
	return &g, "ICD description loaded. Waiting for a matching frame."
}
