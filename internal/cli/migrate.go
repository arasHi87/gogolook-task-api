package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
)

// newMigrateCommand builds the schema subcommands.
//
// They live in the same binary as the service so the image that runs the
// migration is byte-identical to the image that runs the code expecting it.
// A separate migration image is a way to apply one version's schema and then
// start another version's code.
func newMigrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply, roll back or inspect the database schema",
		Long: `Apply, roll back or inspect the database schema.

The migrations are embedded in the binary, so this works from the distroless
image, which has no filesystem to read .sql files from and no shell to copy
them in with.

Concurrent runs are safe: a Postgres advisory lock is held for the duration, so
several replicas starting at once cannot race each other through the same
migration.`,
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		newMigrateUpCommand(),
		newMigrateDownCommand(),
		newMigrateStatusCommand(),
	)
	return cmd
}

func newMigrateUpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply every pending migration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := migrationDSN(cmd)
			if err != nil {
				return err
			}
			if err := postgres.MigrateUp(dsn); err != nil {
				return err
			}
			return report(cmd, dsn, "schema is up to date")
		},
	}
}

func newMigrateDownCommand() *cobra.Command {
	var confirm bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Roll back one migration",
		Long: `Roll back exactly one migration.

One step, not all of them. A command that drops the whole schema is a command
someone eventually runs against the wrong database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirm {
				return fmt.Errorf("rolling back drops data; pass --yes to confirm")
			}
			dsn, err := migrationDSN(cmd)
			if err != nil {
				return err
			}
			if err := postgres.MigrateDown(dsn); err != nil {
				return err
			}
			return report(cmd, dsn, "rolled back one migration")
		},
	}
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm that rolling back may drop data")
	return cmd
}

func newMigrateStatusCommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the applied version and what is pending",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := migrationDSN(cmd)
			if err != nil {
				return err
			}
			st, err := postgres.MigrateStatus(dsn)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			_, err = fmt.Fprintf(out, "version=%d dirty=%t pending=%d\n", st.Version, st.Dirty, st.Pending)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

// migrationDSN resolves the connection string through the same four-layer
// merge everything else uses, so --storage.postgres.dsn, the environment and
// config.yaml all work here too.
func migrationDSN(cmd *cobra.Command) (string, error) {
	res, err := config.Load(config.Options{
		File:  config.FilePath(cmd.Flags()),
		Flags: cmd.Flags(),
	})
	if err != nil {
		return "", err
	}
	if res.Config.Storage.Postgres.DSN == "" {
		return "", fmt.Errorf("no database configured; set %s", config.EnvName("storage.postgres.dsn"))
	}
	return res.Config.Storage.Postgres.DSN, nil
}

// report prints where the schema ended up, so a successful run says something
// an operator can check rather than nothing at all.
func report(cmd *cobra.Command, dsn, msg string) error {
	st, err := postgres.MigrateStatus(dsn)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: version=%d dirty=%t pending=%d\n",
		msg, st.Version, st.Dirty, st.Pending)
	return err
}
