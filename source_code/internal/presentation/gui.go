package presentation

import (
	_ "embed"
	"fmt"
	"image/color"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
	"pbmt/internal/application/logcontrol"
	"pbmt/internal/config"
)

const (
	preferenceThemeMode = "settings.theme_mode"
	preferenceLanguage  = "settings.language"
	preferenceLogLevel  = "settings.log_level"

	themeModeDark   = "Dark"
	themeModeLight  = "Light"
	languageEnglish = "English"
)

//go:embed pbmt_icon.png
var pbmtIconPNG []byte

var pbmtIcon = fyne.NewStaticResource("pbmt_icon.png", pbmtIconPNG)

type moduleID string

const (
	moduleGooseSub  moduleID = "goose_sub"
	moduleGoosePub  moduleID = "goose_pub"
	moduleSVSub     moduleID = "sv_sub"
	moduleSVPub     moduleID = "sv_pub"
	modulePTPClient moduleID = "ptp_client"
	modulePTPServer moduleID = "ptp_server"
)

type uiState struct {
	deps                Dependencies
	app                 fyne.App
	window              fyne.Window
	configPath          string
	cfg                 *config.Config
	statusBar           *widget.Label
	logWarning          *widget.Label
	statuses            map[moduleID]string
	svPublisherStates   map[string]*svPublisherManualState
	manualPopOutWindows map[moduleID]*manualPopOutView
	suppressManualOpen  time.Time
	mainTabs            *container.AppTabs
	configurationTabs   *container.AppTabs
	sclModelView        *sclModelView
	catalogView         *mcpCatalogView
	mcpServer           automation.Server
	mcpToken            string
	dashboard           *fyne.Container
	manualPane          *fyne.Container
	manualStop          func()
	manualOpenModule    moduleID
	runtime             control.Controller
	logLevel            logcontrol.Controller
}

type svPublisherManualState struct {
	BaseFrequency    string
	UseBaseFrequency bool
	Simulation       bool
	CalculateNeutral bool
	Sync             string
	Current          svPublisherGroupState
	Voltage          svPublisherGroupState
}

type svPublisherGroupState struct {
	Mode        string
	BalancedRMS string
	TwoPhaseRMS string
	CompRMS     [3]string
	CompPhase   [3]string
	Rows        map[int]svPublisherRowState
}

type svPublisherRowState struct {
	RMS       string
	Phase     string
	Frequency string
	Quality   string
}

func Run(deps Dependencies) error {
	if err := deps.validate(); err != nil {
		return err
	}
	a := app.NewWithID("pbmt.gui")
	applyThemeMode(a, a.Preferences().StringWithFallback(preferenceThemeMode, themeModeDark))
	logLevelName := logcontrol.Normalize(a.Preferences().StringWithFallback(preferenceLogLevel, logcontrol.LevelInfo))
	a.Preferences().SetString(preferenceLogLevel, logLevelName)
	logLevel := deps.ConfigureLogger(logLevelName)

	w := a.NewWindow("Process Bus Multi Tool")
	w.SetIcon(pbmtIcon)
	w.Resize(fyne.NewSize(1160, 720))

	state := &uiState{
		deps:                deps,
		app:                 a,
		window:              w,
		statuses:            make(map[moduleID]string),
		svPublisherStates:   make(map[string]*svPublisherManualState),
		manualPopOutWindows: make(map[moduleID]*manualPopOutView),
		statusBar:           widget.NewLabel("Create or open project"),
		logLevel:            logLevel,
	}

	w.SetContent(state.build())
	stopLogMonitor := state.monitorLogErrors()
	defer stopLogMonitor()
	w.SetOnClosed(state.shutdown)
	w.ShowAndRun()
	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *uiState) build() fyne.CanvasObject {
	if s.catalogView != nil {
		s.catalogView.dispose()
		s.catalogView = nil
	}
	if s.sclModelView != nil {
		s.sclModelView.dispose()
		s.sclModelView = nil
	}
	s.configurationTabs = nil
	openTab := container.NewTabItemWithIcon("Open Project", theme.FolderOpenIcon(), s.projectBrowserPage())
	manualTab := container.NewTabItemWithIcon("Manual", theme.HomeIcon(), s.manualPage())
	tabs := container.NewAppTabs(
		openTab,
		container.NewTabItemWithIcon("Configuration", theme.SettingsIcon(), s.configurationPage()),
		manualTab,
		container.NewTabItemWithIcon("MCP", theme.ListIcon(), s.mcpCatalogPage()),
		container.NewTabItemWithIcon("Settings", theme.SettingsIcon(), s.settingsPage()),
	)
	tabs.SetTabLocation(container.TabLocationTop)
	s.mainTabs = tabs
	if s.logWarning == nil {
		s.logWarning = widget.NewLabel("")
		s.logWarning.Wrapping = fyne.TextWrapWord
		s.logWarning.Hide()
	}
	return container.NewBorder(nil, container.NewVBox(s.logWarning, s.statusBar), nil, nil, tabs)
}

// File write failures must remain visible even if subsequent operations replace
// the ordinary status text or the selected level filters out diagnostic logs.
func (s *uiState) monitorLogErrors() func() {
	done := make(chan struct{})
	var stopped atomic.Bool
	var queued atomic.Bool
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if queued.Swap(true) {
					continue
				}
				fyne.Do(func() {
					defer queued.Store(false)
					if stopped.Load() {
						return
					}
					s.refreshLogWarning()
				})
			}
		}
	}()
	return func() {
		if !stopped.Swap(true) {
			close(done)
		}
	}
}

func (s *uiState) refreshLogWarning() {
	if s.logLevel == nil || s.logWarning == nil {
		return
	}
	if err := s.logLevel.Err(); err != nil {
		s.logWarning.SetText("Logging incomplete: " + err.Error() + ". Check disk space/permissions and reopen the project.")
		s.logWarning.Show()
	} else {
		s.logWarning.Hide()
	}
}

func (s *uiState) settingsPage() fyne.CanvasObject {
	themeMode := s.app.Preferences().StringWithFallback(preferenceThemeMode, themeModeDark)
	language := s.app.Preferences().StringWithFallback(preferenceLanguage, languageEnglish)
	logLevel := logcontrol.Normalize(s.app.Preferences().StringWithFallback(preferenceLogLevel, logcontrol.LevelInfo))
	if language != languageEnglish {
		language = languageEnglish
		s.app.Preferences().SetString(preferenceLanguage, languageEnglish)
	}

	themeSelect := widget.NewRadioGroup([]string{themeModeLight, themeModeDark}, func(value string) {
		s.app.Preferences().SetString(preferenceThemeMode, value)
		applyThemeMode(s.app, value)
		s.refreshDashboard()
		s.statusBar.SetText("Settings saved")
	})
	themeSelect.Horizontal = true
	themeSelect.SetSelected(themeMode)

	languageSelect := widget.NewSelect([]string{languageEnglish}, func(value string) {
		s.app.Preferences().SetString(preferenceLanguage, value)
		s.statusBar.SetText("Settings saved")
	})
	languageSelect.SetSelected(language)

	logLevelSelect := widget.NewSelect(logcontrol.Levels(), nil)
	logLevelSelect.SetSelected(logLevel)
	logLevelSelect.OnChanged = func(value string) {
		value = logcontrol.Normalize(value)
		s.app.Preferences().SetString(preferenceLogLevel, value)
		if s.logLevel != nil {
			s.logLevel.Set(value)
		}
		slog.Info("log level changed", "level", value)
		s.statusBar.SetText("Settings saved")
	}

	form := widget.NewForm(
		widget.NewFormItem("Language", languageSelect),
		widget.NewFormItem("Theme mode", themeSelect),
		widget.NewFormItem("Log level", logLevelSelect),
	)
	return container.NewPadded(widget.NewCard("Settings", "", form))
}

func applyThemeMode(a fyne.App, mode string) {
	switch mode {
	case themeModeLight:
		a.Settings().SetTheme(theme.LightTheme())
	default:
		a.Settings().SetTheme(theme.DarkTheme())
	}
}

func showError(err error, window fyne.Window) {
	if err == nil {
		return
	}
	slog.Error("UI operation failed", "error", err)
	msg := widget.NewLabel(err.Error())
	msg.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustom("Error", "OK", container.NewPadded(msg), window)
	d.Resize(fyne.NewSize(560, msg.MinSize().Height+120))
	d.Show()
}

func (s *uiState) projectBrowserPage() fyne.CanvasObject {
	currentDir := s.configPath
	if !pathExists(currentDir) {
		if home, err := os.UserHomeDir(); err == nil {
			currentDir = home
		}
	}
	currentDir = filepath.Clean(currentDir)

	currentLabel := widget.NewLabel("")
	selectedLabel := widget.NewLabel("Selected: -")
	var entries []projectBrowserEntry
	var selected string
	var lastSelected string
	var lastSelectedAt time.Time
	var loadDir func(string)

	list := widget.NewList(
		func() int { return len(entries) },
		func() fyne.CanvasObject {
			btn := widget.NewButtonWithIcon("", theme.FolderIcon(), nil)
			btn.Alignment = widget.ButtonAlignLeading
			return btn
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			btn := obj.(*widget.Button)
			entry := entries[id]
			btn.SetText(entry.name)
			if entry.directory {
				btn.SetIcon(theme.FolderIcon())
			} else {
				btn.SetIcon(theme.FileIcon())
			}
			btn.OnTapped = func() {
				selected = entry.name
				selectedLabel.SetText("Selected: " + filepath.Join(currentDir, selected))
				now := time.Now()
				if selected == lastSelected && now.Sub(lastSelectedAt) < 800*time.Millisecond {
					if entry.directory {
						loadDir(filepath.Join(currentDir, selected))
					}
					lastSelected = ""
					lastSelectedAt = time.Time{}
					return
				}
				lastSelected = selected
				lastSelectedAt = now
			}
		},
	)

	loadDir = func(path string) {
		path = filepath.Clean(path)
		directoryEntries, err := os.ReadDir(path)
		if err != nil {
			showError(err, s.window)
			return
		}
		currentDir = path
		currentLabel.SetText("Folder: " + currentDir)
		selected = ""
		selectedLabel.SetText("Selected: -")
		entries = projectBrowserEntries(directoryEntries)
		list.UnselectAll()
		list.Refresh()
	}

	up := widget.NewButtonWithIcon("Up", theme.NavigateBackIcon(), func() {
		parent := filepath.Dir(currentDir)
		if parent != currentDir {
			loadDir(parent)
		}
	})
	use := widget.NewButtonWithIcon("Use As Project", theme.ConfirmIcon(), func() {
		if err := s.loadProject(currentDir); err != nil {
			showError(err, s.window)
			return
		}
	})
	create := widget.NewButtonWithIcon("Create Project", theme.ContentAddIcon(), func() {
		nameEntry := widget.NewEntry()
		nameEntry.SetPlaceHolder("Project name")
		form := widget.NewForm(widget.NewFormItem("Name", nameEntry))
		d := dialog.NewCustomConfirm("Create Project", "OK", "Cancel", form, func(ok bool) {
			if !ok {
				return
			}
			name := strings.TrimSpace(nameEntry.Text)
			if name == "" {
				showError(fmt.Errorf("project name is required"), s.window)
				return
			}
			if strings.ContainsAny(name, `/\:`) {
				showError(fmt.Errorf("project name contains invalid path characters"), s.window)
				return
			}
			dst := filepath.Join(currentDir, name)
			if pathExists(dst) {
				showError(fmt.Errorf("project folder already exists: %s", dst), s.window)
				return
			}
			if err := s.deps.CreateProject(dst); err != nil {
				showError(err, s.window)
				return
			}
			if err := s.loadProject(dst); err != nil {
				showError(err, s.window)
				return
			}
			slog.Info("project created", "path", dst)
		}, s.window)
		d.Resize(fyne.NewSize(420, 140))
		d.Show()
	})

	loadDir(currentDir)
	return container.NewPadded(container.NewBorder(
		container.NewVBox(currentLabel, selectedLabel, container.NewHBox(up, use, create)),
		nil,
		nil,
		nil,
		widget.NewCard("Project Folders and Files", "", list),
	))
}

type projectBrowserEntry struct {
	name      string
	directory bool
}

func projectBrowserEntries(entries []os.DirEntry) []projectBrowserEntry {
	visible := make([]projectBrowserEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			visible = append(visible, projectBrowserEntry{name: entry.Name(), directory: true})
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".yaml" || ext == ".log" {
			visible = append(visible, projectBrowserEntry{name: entry.Name()})
		}
	}
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].directory != visible[j].directory {
			return visible[i].directory
		}
		return strings.ToLower(visible[i].name) < strings.ToLower(visible[j].name)
	})
	return visible
}

func (s *uiState) manualPage() fyne.CanvasObject {
	s.dashboard = container.NewVBox()
	s.manualPane = container.NewStack(s.manualPlaceholder())
	s.refreshDashboard()

	leftMin := canvas.NewRectangle(color.Transparent)
	leftMin.SetMinSize(fyne.NewSize(330, 1))
	left := container.NewStack(leftMin, s.dashboard)
	return container.NewPadded(container.NewBorder(nil, nil, left, nil, s.manualPane))
}

func (s *uiState) configurationPage() fyne.CanvasObject {
	if s.cfg == nil || s.configPath == "" {
		s.configurationTabs = nil
		msg := widget.NewLabel("Create or open a project to edit configuration.")
		msg.Alignment = fyne.TextAlignCenter
		return container.NewPadded(widget.NewCard("Configuration", "", msg))
	}
	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("SCL Model", theme.ComputerIcon(), s.sclModelPage()),
		container.NewTabItemWithIcon("GOOSE Subscriber", theme.DownloadIcon(), s.configEditorPage(moduleGooseSub)),
		container.NewTabItemWithIcon("GOOSE Publisher", theme.UploadIcon(), s.configEditorPage(moduleGoosePub)),
		container.NewTabItemWithIcon("SV Subscriber", theme.DownloadIcon(), s.configEditorPage(moduleSVSub)),
		container.NewTabItemWithIcon("SV Publisher", theme.UploadIcon(), s.configEditorPage(moduleSVPub)),
		container.NewTabItemWithIcon("PTP Client", theme.HistoryIcon(), s.configEditorPage(modulePTPClient)),
		container.NewTabItemWithIcon("PTP Server", theme.ComputerIcon(), s.configEditorPage(modulePTPServer)),
	)
	tabs.SetTabLocation(container.TabLocationTop)
	s.configurationTabs = tabs
	return container.NewPadded(tabs)
}
