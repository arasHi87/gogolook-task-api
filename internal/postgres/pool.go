// Package postgres owns the connection pool and the schema.
//
// Everything here is about the connection surviving contact with reality: a
// pool with no lifetime rotation, no health check and no statement timeout
// works perfectly until a load balancer silently drops an idle connection, or
// one runaway query pins a connection forever.
package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Role names the pool in pg_stat_activity, so "which of our pools is holding
// that connection" is answerable from the database rather than by guessing.
type Role string

const (
	// RoleAPI serves requests.
	RoleAPI Role = "api"
	// RoleWorker consumes jobs. It gets its own pool, which is the bulkhead:
	// a worker pool saturated by slow jobs must not be able to starve the API.
	RoleWorker Role = "worker"
	// RoleMigrate runs the schema migration and then goes away.
	RoleMigrate Role = "migrate"
)

// Open builds a pool from configuration and verifies it can actually connect.
//
// It pings before returning, so a bad DSN or an unreachable database is a
// startup failure with a clear message rather than a confusing error on the
// first request.
func Open(ctx context.Context, cfg config.Postgres, role Role, instance string) (*pgxpool.Pool, error) {
	return OpenTraced(ctx, cfg, role, instance, false)
}

// OpenTraced is Open with a span per statement.
//
// Separate rather than a field on config.Postgres, because whether the process
// exports traces is a property of the deployment and not of the connection —
// and the migrator, which opens a pool to run DDL and then exits, has nothing
// to say to a collector.
func OpenTraced(ctx context.Context, cfg config.Postgres, role Role, instance string, tracing bool) (*pgxpool.Pool, error) {
	pc, err := poolConfig(cfg, role, instance)
	if err != nil {
		return nil, err
	}
	if tracing {
		// The statement text becomes the span name, which is safe here and
		// would not be with string-concatenated SQL: every query in this
		// service is a constant with bind parameters, so the name set is
		// bounded by the code.
		pc.ConnConfig.Tracer = otelpgx.NewTracer(
			otelpgx.WithTrimSQLInSpanName(),
			otelpgx.WithDisableQuerySpanNamePrefix(),
		)
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: build pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout.D())
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	return pool, nil
}

// poolConfig turns configuration into a pgxpool config.
func poolConfig(cfg config.Postgres, role Role, instance string) (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The DSN can contain a password, so the parse error is not passed
		// through: pgx includes the input in its message.
		return nil, fmt.Errorf("postgres: dsn is not parseable")
	}

	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	// Rotation past a proxy's idle reaping, and a bounded window for a
	// failover to drain the connections pointing at the old primary.
	pc.MaxConnLifetime = cfg.MaxConnLifetime.D()
	pc.MaxConnIdleTime = cfg.MaxConnIdleTime.D()
	// pgx pings in the background, so a dead connection is evicted before a
	// request finds it rather than by failing that request.
	pc.HealthCheckPeriod = cfg.HealthCheckPeriod.D()
	pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout.D()

	// Session settings, applied by the server to every connection in the pool.
	//
	// statement_timeout: a runaway query cannot pin a connection forever.
	// idle_in_transaction_session_timeout: a bug that leaves a transaction
	//   open cannot block VACUUM indefinitely, which on the high-churn jobs
	//   table is how a queue becomes a storage incident.
	// lock_timeout: a migration waiting behind a long read fails fast instead
	//   of holding the queue behind it.
	pc.ConnConfig.RuntimeParams["application_name"] = applicationName(role, instance)
	pc.ConnConfig.RuntimeParams["statement_timeout"] = millis(cfg.StatementTimeout)
	pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = millis(cfg.IdleInTransactionSessionTimeout)
	pc.ConnConfig.RuntimeParams["lock_timeout"] = millis(cfg.LockTimeout)

	return pc, nil
}

// applicationName is what shows up in pg_stat_activity.
func applicationName(role Role, instance string) string {
	if instance == "" {
		return "taskapi-" + string(role)
	}
	return "taskapi-" + string(role) + "-" + instance
}

// millis renders a duration the way Postgres wants its timeouts. Zero means no
// limit, which is Postgres's own default and is what an operator gets by
// setting the knob to zero deliberately.
func millis(d config.Duration) string {
	return strconv.FormatInt(int64(d.D()/time.Millisecond), 10)
}
