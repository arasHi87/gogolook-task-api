// Package testenv provides the shared test infrastructure.
//
// It is an ordinary package rather than a test-only one because several
// packages' tests import it. Nothing outside a _test.go file references it, so
// nothing here is linked into a production build.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
)

// templateDB is migrated once and then copied per test.
const templateDB = "taskapi_template"

// Postgres returns a pool against a private, migrated database.
//
// A real database, not a fake: SKIP LOCKED behaviour, lease expiry, partial
// unique indexes and transaction visibility are properties of the database,
// not of our code. A mock would only assert that we called the methods we
// wrote, which is not the question being asked.
//
// Each test gets its own database, copied from a migrated template. Truncating
// a shared one instead looks cheaper and is wrong: tests marked t.Parallel()
// then delete each other's rows, and the failures that produces look like
// application bugs rather than harness bugs.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return open(t, database(t))
}

// DSN returns the connection string of a private, migrated database, for tests
// that drive the migrator or open their own pool.
func DSN(t *testing.T) string {
	t.Helper()
	return dsnFor(t, database(t))
}

// FreshDSN returns a private database with no schema applied, for tests of the
// migrator itself.
func FreshDSN(t *testing.T) string {
	t.Helper()

	admin := adminPool(t)
	name := randomName()
	exec(t, admin, fmt.Sprintf("CREATE DATABASE %q", name))
	t.Cleanup(func() { drop(admin, name) })

	return dsnFor(t, name)
}

// database creates a copy of the migrated template and returns its name.
func database(t *testing.T) string {
	t.Helper()

	admin := adminPool(t)
	name := randomName()

	// TEMPLATE copies the files rather than replaying the migrations, so a
	// per-test database costs milliseconds instead of a schema build.
	exec(t, admin, fmt.Sprintf("CREATE DATABASE %q TEMPLATE %q", name, templateDB))
	t.Cleanup(func() { drop(admin, name) })

	return name
}

func open(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()

	pool, err := postgres.Open(context.Background(), poolConfig(dsnFor(t, name)), postgres.RoleAPI, "test")
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// exec runs a statement that cannot be run inside a transaction.
func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// drop removes a test database. Cleanup runs after the test's own context is
// cancelled, so it uses its own.
func drop(admin *pgxpool.Pool, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name))
}

func randomName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "test_" + hex.EncodeToString(b[:])
}

// dsnFor points the container's connection string at one database.
func dsnFor(t *testing.T, name string) string {
	t.Helper()

	u, err := url.Parse(adminDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// poolConfig returns pool settings suitable for a test: small, and quick to
// fail rather than quick to hang.
func poolConfig(dsn string) config.Postgres {
	pg := config.Defaults().Storage.Postgres
	pg.DSN = dsn
	pg.MaxConns = 10
	pg.MinConns = 0
	pg.ConnectTimeout = config.Duration(10 * time.Second)
	// Long enough that a deliberately slow test query survives, short enough
	// that a genuinely stuck one fails the test rather than hanging it.
	pg.StatementTimeout = config.Duration(20 * time.Second)
	return pg
}

// container is started once per test binary and shared by every test in it.
// Starting one per test would cost far more than the tests do.
var container = sync.OnceValues(startContainer)

func adminDSN(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping: needs Docker (go test -short)")
	}
	dsn, err := container()
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	return dsn
}

// adminPool is the connection used to create and drop test databases. It is
// separate from every test's pool, because a database cannot be dropped while
// something is still connected to it.
var adminPool = func() func(*testing.T) *pgxpool.Pool {
	var (
		once sync.Once
		pool *pgxpool.Pool
		err  error
	)
	return func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		dsn := adminDSN(t)
		once.Do(func() {
			pool, err = postgres.Open(context.Background(), poolConfig(dsn), postgres.RoleMigrate, "testadmin")
		})
		if err != nil {
			t.Fatalf("open admin pool: %v", err)
		}
		return pool
	}
}()

// startContainer boots Postgres and builds the migrated template.
func startContainer() (string, error) {
	ctx := context.Background()

	c, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("taskapi"),
		tcpostgres.WithPassword("taskapi"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return "", err
	}
	// Deliberately not terminated here: testcontainers' reaper removes it when
	// the test process exits, and tearing it down from this goroutine would
	// race the tests still using it.

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", err
	}
	if err := buildTemplate(ctx, dsn); err != nil {
		return "", err
	}
	return dsn, nil
}

// buildTemplate creates the template database and migrates it once.
func buildTemplate(ctx context.Context, admin string) error {
	pool, err := postgres.Open(ctx, poolConfig(admin), postgres.RoleMigrate, "template")
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", templateDB)); err != nil {
		return fmt.Errorf("create template database: %w", err)
	}

	u, err := url.Parse(admin)
	if err != nil {
		return err
	}
	u.Path = "/" + templateDB

	if err := postgres.MigrateUp(u.String()); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}
