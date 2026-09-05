// Package cli is the command-line surface: flag parsing, configuration
// bootstrap and the subcommand table. It owns no business logic — every
// command's job is to build a config and a logger and hand them to
// internal/app.
package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/arasHi87/gogolook-task-api/internal/app"
	"github.com/arasHi87/gogolook-task-api/internal/buildinfo"
	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/logging"
)

// NewRootCommand builds the command tree.
//
// Long flags are POSIX two-dash only. `-config` does not parse and is not meant
// to: that is pflag's deliberate behaviour, and mixing the two conventions in
// one binary is worse than committing to one.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "taskapi",
		Short: "Task API — HTTP service and Postgres-backed job queue",
		Long: `taskapi serves the Task API and consumes its job queue.

The default storage backend is in-memory, so

    go run ./cmd/taskapi all

brings the whole API up with no Postgres, no Docker and no configuration.
Point --storage.backend at postgres to get durability, the transactional
outbox and the queue.`,
		SilenceUsage:  true,
		SilenceErrors: false,
		// Without this, cobra prints usage for runtime failures too, burying a
		// one-line "connection refused" under sixty lines of flag help.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return nil
		},
	}

	config.RegisterFlags(root.PersistentFlags())

	root.AddCommand(
		newServeCommand(),
		newWorkerCommand(),
		newAllCommand(),
		newMigrateCommand(),
		newVersionCommand(),
		newHealthcheckCommand(),
	)
	return root
}

// booted is everything a run command needs, in the order it was built.
type booted struct {
	cfg  *config.Config
	log  *logging.Handle
	file string
}

// boot performs the configuration and logging bootstrap shared by every run
// command: merge the layers, check the result, build the logger.
func boot(cmd *cobra.Command) (*booted, error) {
	fs := cmd.Flags()

	res, err := config.Load(config.Options{File: config.FilePath(fs), Flags: fs})
	if err != nil {
		return nil, err
	}
	if err := res.Config.Validate(); err != nil {
		return nil, err
	}

	log, err := logging.New(logging.Options{
		Level:   res.Config.Logging.Level,
		Format:  res.Config.Logging.Format,
		Service: res.Config.Service.Name,
		Version: buildinfo.Version(),
	})
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		log.Warn("configuration warning", "detail", w)
	}

	return &booted{cfg: res.Config, log: log, file: res.File}, nil
}

// printConfig dumps the effective merged configuration, with secrets masked.
//
// It goes to stdout, which is reserved for machine-readable output precisely so
// this can be piped while the log stream stays on stderr.
func printConfig(cmd *cobra.Command) error {
	res, err := config.Load(config.Options{File: config.FilePath(cmd.Flags()), Flags: cmd.Flags()})
	if err != nil {
		return err
	}
	if err := res.Config.Validate(); err != nil {
		return err
	}
	return res.Config.WriteYAML(cmd.OutOrStdout())
}

// runMode is the body of serve, worker and all: bootstrap, wire, run until a
// signal, drain.
func runMode(cmd *cobra.Command, mode app.Mode) error {
	if config.PrintConfigRequested(cmd.Flags()) {
		return printConfig(cmd)
	}

	b, err := boot(cmd)
	if err != nil {
		return err
	}

	a, err := app.New(app.Options{
		Mode:       mode,
		Config:     b.cfg,
		ConfigFile: b.file,
		Flags:      cmd.Flags(),
		Logger:     b.log,
	})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := a.Close(); cerr != nil {
			b.log.Error("cleanup failed", "err", cerr)
		}
	}()

	// SIGINT and SIGTERM start the drain; SIGHUP is handled by the reloader
	// worker inside the app, not here.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("%s: %w", mode, err)
	}
	return nil
}

func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API only (consumes no jobs)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMode(cmd, app.ModeServe) },
	}
}

func newWorkerCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the queue consumer only (no public listener; admin listener stays up)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMode(cmd, app.ModeWorker) },
	}
}

func newAllCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "all",
		Short: "Run the API and the queue consumer in one process (the default for local runs)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runMode(cmd, app.ModeAll) },
	}
}
