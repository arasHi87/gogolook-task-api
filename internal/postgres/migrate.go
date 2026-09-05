package postgres

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/arasHi87/gogolook-task-api/migrations"
)

// Status is what `migrate status` reports.
type Status struct {
	// Version is the migration currently applied, or 0 for a fresh database.
	Version uint `json:"version"`
	// Dirty means a migration failed partway and the schema is in an unknown
	// state. Nothing may run until a human resolves it, which is why it is
	// reported rather than repaired automatically.
	Dirty bool `json:"dirty"`
	// Pending is how many migrations have not been applied.
	Pending int `json:"pending"`
}

// migrator opens the embedded migrations against a database.
//
// golang-migrate takes a pg_advisory_lock for the duration, so several
// replicas starting at once cannot race each other through the same
// migration. That matters here because compose and Kubernetes both start
// every replica simultaneously.
func migrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("postgres: read embedded migrations: %w", err)
	}

	// The pgx/v5 URL scheme, so there is one Postgres driver in the binary
	// rather than lib/pq alongside it.
	m, err := migrate.NewWithSourceInstance("iofs", src, pgxURL(dsn))
	if err != nil {
		return nil, fmt.Errorf("postgres: open migrator: %w", err)
	}
	return m, nil
}

// MigrateUp applies every pending migration. Applying nothing is success.
func MigrateUp(dsn string) error {
	m, err := migrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("postgres: migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back one migration.
//
// One step, not all of them: `migrate down` that drops the whole schema is a
// command someone eventually runs against the wrong database.
func MigrateDown(dsn string) error {
	m, err := migrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("postgres: migrate down: %w", err)
	}
	return nil
}

// MigrateStatus reports what is applied and what is not.
func MigrateStatus(dsn string) (Status, error) {
	m, err := migrator(dsn)
	if err != nil {
		return Status{}, err
	}
	defer closeMigrator(m)

	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return Status{}, fmt.Errorf("postgres: migrate status: %w", err)
	}

	pending, err := countPending(version)
	if err != nil {
		return Status{}, err
	}
	return Status{Version: version, Dirty: dirty, Pending: pending}, nil
}

// countPending counts migrations newer than the applied version.
func countPending(applied uint) (int, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("postgres: read embedded migrations: %w", err)
	}

	var pending int
	for _, e := range entries {
		version, ok := upMigrationVersion(e.Name())
		if ok && version > applied {
			pending++
		}
	}
	return pending, nil
}

// closeMigrator reports nothing: the migration already succeeded or failed,
// and a close error on the way out cannot change that.
func closeMigrator(m *migrate.Migrate) {
	srcErr, dbErr := m.Close()
	_, _ = srcErr, dbErr
}

// pgxURL rewrites the DSN's scheme so golang-migrate selects the pgx/v5
// driver. A keyword/value DSN is passed through unchanged; the driver
// registration below accepts it.
func pgxURL(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(dsn) >= len(prefix) && dsn[:len(prefix)] == prefix {
			return "pgx5://" + dsn[len(prefix):]
		}
	}
	return dsn
}

// Referencing the driver keeps the import, which is what registers "pgx5".
var _ = pgxdriver.Postgres{}
