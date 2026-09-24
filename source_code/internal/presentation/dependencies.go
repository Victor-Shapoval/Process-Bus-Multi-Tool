package presentation

import (
	"errors"

	"pbmt/internal/application/automation"
	"pbmt/internal/application/control"
	"pbmt/internal/application/logcontrol"
	"pbmt/internal/application/sclmodel"
	"pbmt/internal/config"
)

type Dependencies struct {
	StartMCP               func(string, *config.Config, control.AgentProvider, automation.ServerOptions) (automation.Server, error)
	DialModel              sclmodel.Dialer
	NewController          func(*config.Config) control.Controller
	CreateProject          func(string) error
	LoadConfigItemTemplate func(string) ([]byte, error)
	ConfigureLogger        func(string) logcontrol.Controller
}

func (d Dependencies) validate() error {
	switch {
	case d.NewController == nil:
		return errors.New("presentation: controller factory is required")
	case d.CreateProject == nil:
		return errors.New("presentation: project factory is required")
	case d.LoadConfigItemTemplate == nil:
		return errors.New("presentation: config item template loader is required")
	case d.ConfigureLogger == nil:
		return errors.New("presentation: logger factory is required")
	default:
		return nil
	}
}
