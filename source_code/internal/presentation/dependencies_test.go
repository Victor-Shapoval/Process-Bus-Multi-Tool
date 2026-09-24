package presentation

import (
	"testing"

	"pbmt/internal/application/control"
	"pbmt/internal/application/logcontrol"
	"pbmt/internal/config"
)

func TestDependenciesRequireEveryCompositionPort(t *testing.T) {
	valid := Dependencies{
		NewController: func(*config.Config) control.Controller { return nil },
		CreateProject: func(string) error { return nil },
		LoadConfigItemTemplate: func(string) ([]byte, error) {
			return []byte("name: template\n"), nil
		},
		ConfigureLogger: func(string) logcontrol.Controller { return nil },
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid dependencies rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Dependencies)
	}{
		{"controller", func(d *Dependencies) { d.NewController = nil }},
		{"project", func(d *Dependencies) { d.CreateProject = nil }},
		{"template", func(d *Dependencies) { d.LoadConfigItemTemplate = nil }},
		{"logger", func(d *Dependencies) { d.ConfigureLogger = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := valid
			tc.mutate(&deps)
			if err := deps.validate(); err == nil {
				t.Fatal("missing dependency was accepted")
			}
		})
	}
}
