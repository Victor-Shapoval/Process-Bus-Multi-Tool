package presentation

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"pbmt/internal/application/control"
	"pbmt/internal/config"
	"pbmt/internal/infrastructure/logging"
	"pbmt/profiles"
)

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestLogWriteErrorRemainsVisibleUntilReopen(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	logger, levels := logging.New(failedLogWriter{}, logging.LevelError)
	defer levels.Close()
	s := &uiState{logLevel: levels, logWarning: widget.NewLabel(""), statusBar: widget.NewLabel("Module running")}
	logger.Error("test write")
	s.refreshLogWarning()
	if !s.logWarning.Visible() || !strings.Contains(s.logWarning.Text, "disk full") {
		t.Fatal("file write error was not visible")
	}
	s.statusBar.SetText("Settings saved")
	s.refreshLogWarning()
	if !s.logWarning.Visible() {
		t.Fatal("ordinary status update hid the log error")
	}
	if err := levels.SetOutputFile(filepath.Join(t.TempDir(), "project.log"), nil); err != nil {
		t.Fatal(err)
	}
	s.refreshLogWarning()
	if s.logWarning.Visible() {
		t.Fatal("successful reopen did not clear the error banner")
	}
}

type projectLoggingRuntime struct {
	control.Controller
	closed bool
}

func (*projectLoggingRuntime) Status(id control.ModuleID) control.ModuleStatus {
	return control.ModuleStatus{ID: id, State: control.StatusStopped}
}

func (r *projectLoggingRuntime) Close() error {
	r.closed = true
	slog.Info("previous runtime finished")
	return nil
}

func TestProjectSwitchKeepsOldShutdownOutOfNewLog(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()
	previousLogger := slog.Default()
	defer slog.SetDefault(previousLogger)
	logger, levels := logging.New(io.Discard, logging.LevelInfo)
	slog.SetDefault(logger)
	s := &uiState{app: app, window: app.NewWindow("logging"), statusBar: widget.NewLabel(""), logLevel: levels,
		deps: Dependencies{NewController: func(*config.Config) control.Controller { return &projectLoggingRuntime{} }, LoadConfigItemTemplate: profiles.DefaultItemTemplate},
	}
	defer s.shutdown()
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	badPath := filepath.Join(root, "bad")
	for _, path := range []string{oldPath, newPath, badPath} {
		if err := profiles.CreateProject(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.loadProject(oldPath); err != nil {
		t.Fatal(err)
	}
	oldRuntime := s.runtime.(*projectLoggingRuntime)
	if err := os.Mkdir(filepath.Join(badPath, "project.log"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.loadProject(badPath); err == nil {
		t.Fatal("unwritable log accepted")
	}
	if oldRuntime.closed || s.configPath != oldPath || s.runtime != oldRuntime {
		t.Fatal("failed project open changed the running project")
	}
	if err := s.loadProject(newPath); err != nil {
		t.Fatal(err)
	}
	if !oldRuntime.closed || s.runtime == oldRuntime {
		t.Fatal("old runtime was retained after project switch")
	}
	oldData, err := os.ReadFile(filepath.Join(oldPath, "project.log"))
	if err != nil {
		t.Fatal(err)
	}
	newData, err := os.ReadFile(filepath.Join(newPath, "project.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oldData), "previous runtime finished") || !strings.Contains(string(oldData), "project closed") || strings.Contains(string(newData), "previous runtime finished") || !strings.Contains(string(newData), "project loaded") {
		t.Fatalf("project logs mixed: old=%s new=%s", oldData, newData)
	}
}
