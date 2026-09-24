// Package logging provides a shared configurable slog logger for all pbmt modules.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"pbmt/internal/application/logcontrol"
)

const (
	LevelError = logcontrol.LevelError
	LevelWarn  = logcontrol.LevelWarn
	LevelInfo  = logcontrol.LevelInfo
	LevelDebug = logcontrol.LevelDebug
)

// LevelController changes the minimum level of an existing slog.Handler.
// Loggers derived with logger.With use the same controller.
type LevelController struct {
	level  *slog.LevelVar
	output *switchWriter
}

type switchWriter struct {
	mu     sync.Mutex
	writer io.Writer
	closer io.Closer
	err    error
}

// New creates a text logger whose level can be changed without restarting.
func New(w io.Writer, level string) (*slog.Logger, *LevelController) {
	if w == nil {
		w = io.Discard
	}
	levelVar := &slog.LevelVar{}
	output := &switchWriter{writer: w}
	controller := &LevelController{level: levelVar, output: output}
	controller.Set(level)
	handler := slog.NewTextHandler(output, &slog.HandlerOptions{Level: levelVar})
	return slog.New(handler), controller
}

// NormalizeLevel converts a value to a supported form and falls back to INFO.
func NormalizeLevel(level string) string {
	return logcontrol.Normalize(level)
}

// Set applies a level and returns its normalized name.
func (c *LevelController) Set(level string) string {
	level = NormalizeLevel(level)
	if c == nil || c.level == nil {
		return level
	}
	switch level {
	case LevelError:
		c.level.Set(slog.LevelError)
	case LevelWarn:
		c.level.Set(slog.LevelWarn)
	case LevelDebug:
		c.level.Set(slog.LevelDebug)
	default:
		c.level.Set(slog.LevelInfo)
	}
	return level
}

// SetOutputFile atomically redirects the existing logger and every derived
// logger.With instance to an append-only file. The current output remains
// active if the new file cannot be opened.
func (c *LevelController) SetOutputFile(path string, beforeSwitch func()) error {
	if c == nil || c.output == nil {
		return fmt.Errorf("logging: controller is not initialized")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", path, err)
	}
	if beforeSwitch != nil {
		beforeSwitch()
	}
	c.output.replace(file, file)
	return nil
}

// Err exposes write failures because slog's convenience methods do not return
// handler errors. The GUI reports them independently of the selected log level.
func (c *LevelController) Err() error {
	if c == nil || c.output == nil {
		return nil
	}
	c.output.mu.Lock()
	defer c.output.mu.Unlock()
	return c.output.err
}

// Close releases the active file, if any, and discards subsequent records.
func (c *LevelController) Close() error {
	if c == nil || c.output == nil {
		return nil
	}
	return c.output.close()
}

func (w *switchWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if w.err == nil {
			_, _ = fmt.Fprintf(os.Stderr, "PBMT: project log write failed: %v\n", err)
		}
		w.err = fmt.Errorf("project log write failed: %w", err)
		// Do not recursively use slog to report a broken log destination.
		_, _ = os.Stderr.Write(data)
	}
	return n, err
}

func (w *switchWriter) replace(writer io.Writer, closer io.Closer) {
	if writer == nil {
		writer = io.Discard
	}
	w.mu.Lock()
	previous := w.closer
	w.writer = writer
	w.closer = closer
	w.err = nil
	if previous != nil {
		if err := previous.Close(); err != nil {
			w.err = fmt.Errorf("close previous project log: %w", err)
			_, _ = fmt.Fprintln(os.Stderr, w.err)
		}
	}
	w.mu.Unlock()
}

func (w *switchWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	previous := w.closer
	w.writer = io.Discard
	w.closer = nil
	if previous == nil {
		return nil
	}
	return previous.Close()
}
