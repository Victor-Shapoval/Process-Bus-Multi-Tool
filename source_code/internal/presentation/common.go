package presentation

import (
	"image/color"
	"log/slog"
	"net"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"

	pbmtruntime "pbmt/internal/application/control"
)

func moduleTitle(id moduleID) string {
	switch id {
	case moduleGooseSub:
		return "GOOSE Subscriber"
	case moduleGoosePub:
		return "GOOSE Publisher"
	case moduleSVSub:
		return "SV Subscriber"
	case moduleSVPub:
		return "SV Publisher"
	case modulePTPClient:
		return "PTP Client"
	case modulePTPServer:
		return "PTP Server"
	default:
		return string(id)
	}
}

func moduleSubtitle(id moduleID) string {
	switch id {
	case moduleGooseSub:
		return "Discrete signal monitor"
	case moduleGoosePub:
		return "Discrete signal control"
	case moduleSVSub:
		return "Analog stream monitor"
	case moduleSVPub:
		return "Analog value control"
	case modulePTPClient:
		return "Time sync slave"
	case modulePTPServer:
		return "Grandmaster clock"
	default:
		return ""
	}
}

func moduleIcon(id moduleID) fyne.Resource {
	switch id {
	case moduleGooseSub:
		return theme.DownloadIcon()
	case moduleGoosePub:
		return theme.UploadIcon()
	case moduleSVSub:
		return theme.DownloadIcon()
	case moduleSVPub:
		return theme.UploadIcon()
	case modulePTPClient:
		return theme.HistoryIcon()
	case modulePTPServer:
		return theme.ComputerIcon()
	default:
		return theme.SearchIcon()
	}
}

func (s *uiState) statusColor(status string) color.Color {
	light := s.app != nil && s.app.Preferences().StringWithFallback(preferenceThemeMode, themeModeDark) == themeModeLight
	if light {
		switch status {
		case "Running":
			return color.NRGBA{R: 188, G: 222, B: 199, A: 255}
		case "Error":
			return color.NRGBA{R: 232, G: 190, B: 190, A: 255}
		default:
			return color.NRGBA{R: 188, G: 211, B: 231, A: 255}
		}
	}
	switch status {
	case "Running":
		return color.NRGBA{R: 31, G: 92, B: 58, A: 255}
	case "Error":
		return color.NRGBA{R: 120, G: 47, B: 47, A: 255}
	default:
		return color.NRGBA{R: 31, G: 78, B: 119, A: 255}
	}
}

func runtimeModuleID(id moduleID) (pbmtruntime.ModuleID, bool) {
	switch id {
	case moduleGooseSub:
		return pbmtruntime.ModuleGooseSub, true
	case moduleGoosePub:
		return pbmtruntime.ModuleGoosePub, true
	case moduleSVSub:
		return pbmtruntime.ModuleSVSub, true
	case moduleSVPub:
		return pbmtruntime.ModuleSVPub, true
	case modulePTPClient:
		return pbmtruntime.ModulePTPClient, true
	case modulePTPServer:
		return pbmtruntime.ModulePTPServer, true
	default:
		return "", false
	}
}

func networkInterfaceNames() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Error("network interface enumeration failed", "error", err)
		return nil
	}
	names := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		names = append(names, iface.Name)
	}
	sort.Strings(names)
	return names
}

func stringInSlice(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
