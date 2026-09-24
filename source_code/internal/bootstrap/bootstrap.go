// Package bootstrap is the composition root for the desktop application.
package bootstrap

import (
	"log/slog"
	"os"

	"pbmt/internal/application/control"
	"pbmt/internal/application/logcontrol"
	"pbmt/internal/config"
	"pbmt/internal/infrastructure/logging"
	"pbmt/internal/infrastructure/mcpserver"
	"pbmt/internal/infrastructure/mmsclient"
	"pbmt/internal/presentation"
	"pbmt/internal/supervisor"
	"pbmt/profiles"
)

func Run() error {
	return presentation.Run(presentation.Dependencies{
		StartMCP:  mcpserver.Start,
		DialModel: mmsclient.Dial,
		NewController: func(cfg *config.Config) control.Controller {
			return supervisor.New(cfg, slog.Default())
		},
		CreateProject:          profiles.CreateProject,
		LoadConfigItemTemplate: profiles.DefaultItemTemplate,
		ConfigureLogger: func(level string) logcontrol.Controller {
			logger, controller := logging.New(os.Stderr, level)
			slog.SetDefault(logger)
			return controller
		},
	})
}
