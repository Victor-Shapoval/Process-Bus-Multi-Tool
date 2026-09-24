package logging

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeLevel(t *testing.T) {
	tests := map[string]string{
		"error":  LevelError,
		" WARN ": LevelWarn,
		"info":   LevelInfo,
		"debug":  LevelDebug,
		"trace":  LevelInfo,
		"":       LevelInfo,
	}
	for input, want := range tests {
		if got := NormalizeLevel(input); got != want {
			t.Errorf("NormalizeLevel(%q): got %q, want %q", input, got, want)
		}
	}
}

func TestProjectSwitchLogsShutdownToOldFile(t *testing.T) {
	logger, controller := New(io.Discard, LevelInfo)
	defer controller.Close()
	oldPath := filepath.Join(t.TempDir(), "old.log")
	newPath := filepath.Join(t.TempDir(), "new.log")
	if err := controller.SetOutputFile(oldPath, nil); err != nil {
		t.Fatal(err)
	}
	derived := logger.With("module", "test")
	if err := controller.SetOutputFile(newPath, func() {
		derived.Info("old module stopped")
	}); err != nil {
		t.Fatal(err)
	}
	derived.Info("new module started")
	oldData, _ := os.ReadFile(oldPath)
	newData, _ := os.ReadFile(newPath)
	if !strings.Contains(string(oldData), "old module stopped") || strings.Contains(string(oldData), "new module started") || !strings.Contains(string(newData), "new module started") || strings.Contains(string(newData), "old module stopped") {
		t.Fatalf("cross-project logging: old=%s new=%s", oldData, newData)
	}
	called := false
	if err := controller.SetOutputFile(filepath.Join(t.TempDir(), "missing", "project.log"), func() { called = true }); err == nil || called {
		t.Fatal("failed destination must not stop the old project")
	}
}

type shortLogWriter struct{}

func (shortLogWriter) Write([]byte) (int, error) { return 0, nil }

func TestLogWriteFailuresAreObservableAndReopenRecovers(t *testing.T) {
	logger, controller := New(shortLogWriter{}, LevelError)
	defer controller.Close()
	logger.Error("failed record")
	if !errors.Is(controller.Err(), io.ErrShortWrite) {
		t.Fatalf("short write not reported: %v", controller.Err())
	}
	path := filepath.Join(t.TempDir(), "project.log")
	if err := controller.SetOutputFile(path, nil); err != nil {
		t.Fatal(err)
	}
	logger.Error("recovered record")
	if err := controller.Err(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "recovered record") {
		t.Fatal("reopened log is not writable")
	}
	if err := controller.output.closer.Close(); err != nil {
		t.Fatal(err)
	}
	logger.Error("closed file record")
	if controller.Err() == nil {
		t.Fatal("closed-file write failure was hidden")
	}
}

func TestConcurrentDerivedLoggersKeepWholeRecords(t *testing.T) {
	var output bytes.Buffer
	logger, controller := New(&output, LevelInfo)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 40; n++ {
				logger.With("worker", i).Info("record", "number", n)
			}
		}(i)
	}
	wg.Wait()
	if count := strings.Count(output.String(), "\n"); count != 320 {
		t.Fatalf("lost/interleaved records: %d", count)
	}
	if err := controller.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLevelCanBeChangedWithoutRecreatingLogger(t *testing.T) {
	var output bytes.Buffer
	logger, controller := New(&output, LevelInfo)
	derived := logger.With("module", "test")

	derived.Debug("hidden debug")
	derived.Info("visible info")
	if text := output.String(); strings.Contains(text, "hidden debug") || !strings.Contains(text, "visible info") {
		t.Fatalf("INFO filtering produced %q", text)
	}

	output.Reset()
	controller.Set(LevelError)
	derived.Warn("hidden warning")
	derived.Error("visible error")
	if text := output.String(); strings.Contains(text, "hidden warning") || !strings.Contains(text, "visible error") {
		t.Fatalf("ERROR filtering produced %q", text)
	}
}

func TestLevelThresholds(t *testing.T) {
	tests := []struct {
		level   string
		visible []string
		hidden  []string
	}{
		{level: LevelError, visible: []string{"error record"}, hidden: []string{"warn record", "info record", "debug record"}},
		{level: LevelWarn, visible: []string{"error record", "warn record"}, hidden: []string{"info record", "debug record"}},
		{level: LevelInfo, visible: []string{"error record", "warn record", "info record"}, hidden: []string{"debug record"}},
		{level: LevelDebug, visible: []string{"error record", "warn record", "info record", "debug record"}},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			var output bytes.Buffer
			logger, _ := New(&output, tt.level)
			logger.Debug("debug record")
			logger.Info("info record")
			logger.Warn("warn record")
			logger.Error("error record")

			text := output.String()
			for _, message := range tt.visible {
				if !strings.Contains(text, message) {
					t.Errorf("%s filtered required message %q: %q", tt.level, message, text)
				}
			}
			for _, message := range tt.hidden {
				if strings.Contains(text, message) {
					t.Errorf("%s passed filtered message %q: %q", tt.level, message, text)
				}
			}
		})
	}
}

func TestOutputCanSwitchToAppendOnlyFile(t *testing.T) {
	var initial bytes.Buffer
	logger, controller := New(&initial, LevelInfo)
	derived := logger.With("module", "test")
	derived.Info("before project")

	path := filepath.Join(t.TempDir(), "project.log")
	if err := controller.SetOutputFile(path, nil); err != nil {
		t.Fatal(err)
	}
	derived.Info("first project record")
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}

	logger, controller = New(io.Discard, LevelInfo)
	if err := controller.SetOutputFile(path, nil); err != nil {
		t.Fatal(err)
	}
	logger.Info("second project record")
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "before project") ||
		!strings.Contains(text, "first project record") ||
		!strings.Contains(text, "second project record") {
		t.Fatalf("unexpected project log contents: %q", text)
	}
}

func TestFailedOutputSwitchKeepsCurrentWriter(t *testing.T) {
	var output bytes.Buffer
	logger, controller := New(&output, LevelInfo)
	path := filepath.Join(t.TempDir(), "missing", "project.log")
	if err := controller.SetOutputFile(path, nil); err == nil {
		t.Fatal("missing parent directory was accepted")
	}
	logger.Info("still visible")
	if !strings.Contains(output.String(), "still visible") {
		t.Fatalf("current output was lost after failed switch: %q", output.String())
	}
}
