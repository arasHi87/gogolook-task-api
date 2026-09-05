package postgres_test

import (
	"strings"
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

// The session settings are the point of the pool. Each one exists because its
// absence is a documented way to lose a database, so each one is checked
// against the server rather than against the struct we set it on.
func TestSessionSettingsReachTheServer(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults().Storage.Postgres
	cfg.DSN = testenv.DSN(t)
	cfg.StatementTimeout = config.Duration(7 * time.Second)
	cfg.IdleInTransactionSessionTimeout = config.Duration(11 * time.Second)
	cfg.LockTimeout = config.Duration(3 * time.Second)

	pool, err := postgres.Open(t.Context(), cfg, postgres.RoleAPI, "settings")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	want := map[string]string{
		"statement_timeout":                   "7s",
		"idle_in_transaction_session_timeout": "11s",
		"lock_timeout":                        "3s",
	}
	for setting, expected := range want {
		var got string
		if err := pool.QueryRow(t.Context(), "SELECT current_setting($1)", setting).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", setting, err)
		}
		if got != expected {
			t.Errorf("%s = %q, want %q", setting, got, expected)
		}
	}
}

// application_name is what makes "which of our pools is holding that
// connection" answerable from pg_stat_activity rather than by guessing.
func TestApplicationNameIdentifiesRoleAndInstance(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults().Storage.Postgres
	cfg.DSN = testenv.DSN(t)

	pool, err := postgres.Open(t.Context(), cfg, postgres.RoleWorker, "replica-3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	var name string
	if err := pool.QueryRow(t.Context(), "SELECT current_setting('application_name')").Scan(&name); err != nil {
		t.Fatalf("read application_name: %v", err)
	}
	for _, want := range []string{"taskapi", "worker", "replica-3"} {
		if !strings.Contains(name, want) {
			t.Errorf("application_name = %q, want it to contain %q", name, want)
		}
	}
}

// A statement that runs past the timeout must be cut off, or one runaway query
// pins a connection forever.
func TestStatementTimeoutIsEnforced(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults().Storage.Postgres
	cfg.DSN = testenv.DSN(t)
	cfg.StatementTimeout = config.Duration(250 * time.Millisecond)

	pool, err := postgres.Open(t.Context(), cfg, postgres.RoleAPI, "timeout")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(t.Context(), "SELECT pg_sleep(3)")
	if err == nil {
		t.Fatal("a three-second query survived a 250ms statement timeout")
	}
	if !strings.Contains(err.Error(), "statement timeout") {
		t.Errorf("err = %v, want a statement timeout", err)
	}
}

// An unreachable database is a startup failure with a clear message, not a
// confusing error on the first request.
func TestOpenFailsFastOnAnUnreachableDatabase(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults().Storage.Postgres
	cfg.DSN = "postgres://nobody:nothing@127.0.0.1:1/nowhere?sslmode=disable"
	cfg.ConnectTimeout = config.Duration(2 * time.Second)

	started := time.Now()
	pool, err := postgres.Open(t.Context(), cfg, postgres.RoleAPI, "unreachable")
	if err == nil {
		pool.Close()
		t.Fatal("Open succeeded against a database that is not there")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("Open took %v to fail; the connect timeout is not being applied", elapsed)
	}
}

// The DSN carries a password, and pgx puts its input in the parse error.
func TestOpenDoesNotEchoTheDSN(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults().Storage.Postgres
	cfg.DSN = "postgres://taskapi:hunter2@%%%invalid/db"

	_, err := postgres.Open(t.Context(), cfg, postgres.RoleAPI, "bad")
	if err == nil {
		t.Fatal("an unparseable DSN was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error leaked the password: %v", err)
	}
}
