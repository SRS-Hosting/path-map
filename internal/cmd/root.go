// Package cmd wires configuration loading to the server.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/USA-RedDragon/configulator"
	"github.com/USA-RedDragon/path-map/internal/config"
	"github.com/USA-RedDragon/path-map/internal/server"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
)

// New returns the root command.
func New(version string, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path-map",
		Short: "Live player map for a Path of Titans server",
		Long: "path-map polls a Path of Titans dedicated server over RCON and serves a\n" +
			"live, browser-viewable map of everyone online. Polling is demand-driven:\n" +
			"an unwatched map costs the game server nothing.",
		Version: fmt.Sprintf("%s (%s)", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         run,
	}

	return cmd
}

func run(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	// configulator skips a config file it cannot find, which is right for the
	// default path but wrong for one the operator named explicitly: a typo'd
	// --config would otherwise start silently on defaults, looking like it worked.
	if flag := cmd.Flags().Lookup(configulator.ConfigFileKey); flag != nil && flag.Changed {
		if _, err := os.Stat(flag.Value.String()); err != nil {
			return fmt.Errorf("config file %s: %w", flag.Value.String(), err)
		}
	}

	c, err := configulator.FromContext[config.Config](ctx)
	if err != nil {
		return fmt.Errorf("failed to get config from context")
	}

	cfg, err := c.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var logger *slog.Logger
	switch cfg.LogLevel {
	case config.LogLevelDebug:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewTextHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	slog.Info("path-map", "version", cmd.Annotations["version"], "commit", cmd.Annotations["commit"])

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to build server: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop()
	}()

	return srv.Run(ctx)
}
