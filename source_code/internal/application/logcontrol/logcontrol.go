// Package logcontrol defines log-level names and the presentation-facing port
// for changing the process logger and its project output without coupling the
// GUI to its adapter.
package logcontrol

import "strings"

const (
	LevelError = "ERROR"
	LevelWarn  = "WARN"
	LevelInfo  = "INFO"
	LevelDebug = "DEBUG"
)

type Controller interface {
	Set(string) string
	// SetOutputFile opens the destination first, then runs beforeSwitch while
	// records still belong to the previous project. It must not be called concurrently.
	SetOutputFile(path string, beforeSwitch func()) error
	Err() error
	Close() error
}

func Levels() []string {
	return []string{LevelError, LevelWarn, LevelInfo, LevelDebug}
}

func Normalize(level string) string {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case LevelError:
		return LevelError
	case LevelWarn:
		return LevelWarn
	case LevelDebug:
		return LevelDebug
	default:
		return LevelInfo
	}
}
