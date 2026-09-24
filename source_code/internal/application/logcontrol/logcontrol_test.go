package logcontrol

import "testing"

func TestNormalize(t *testing.T) {
	for input, want := range map[string]string{
		" error ": LevelError,
		"warn":    LevelWarn,
		"DEBUG":   LevelDebug,
		"trace":   LevelInfo,
	} {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q): want %q, got %q", input, want, got)
		}
	}
}
