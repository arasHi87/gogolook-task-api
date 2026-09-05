package postgres_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/postgres"
	"github.com/arasHi87/gogolook-task-api/internal/testenv"
)

func TestMigrateUpIsIdempotent(t *testing.T) {
	t.Parallel()
	dsn := testenv.FreshDSN(t)

	before, err := postgres.MigrateStatus(dsn)
	if err != nil {
		t.Fatalf("status on a fresh database: %v", err)
	}
	if before.Version != 0 || before.Pending == 0 {
		t.Fatalf("fresh database reports %+v, want version 0 with work pending", before)
	}

	if err := postgres.MigrateUp(dsn); err != nil {
		t.Fatalf("first up: %v", err)
	}
	// Applying nothing is success. A second run happens on every deploy.
	if err := postgres.MigrateUp(dsn); err != nil {
		t.Fatalf("second up: %v", err)
	}

	after, err := postgres.MigrateStatus(dsn)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if after.Version == 0 {
		t.Error("nothing was applied")
	}
	if after.Pending != 0 {
		t.Errorf("pending = %d after up, want 0", after.Pending)
	}
	if after.Dirty {
		t.Error("the schema is dirty after a clean run")
	}
}

func TestMigrateDownRollsBackOneStep(t *testing.T) {
	t.Parallel()
	dsn := testenv.FreshDSN(t)

	if err := postgres.MigrateUp(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := postgres.MigrateDown(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}

	st, err := postgres.MigrateStatus(dsn)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Version != 0 {
		t.Errorf("version = %d after rolling back the only migration, want 0", st.Version)
	}
	if st.Pending == 0 {
		t.Error("nothing is pending after a rollback")
	}

	// And it goes back up: a down migration that cannot be re-applied is a
	// down migration nobody will ever dare run.
	if err := postgres.MigrateUp(dsn); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

// The migrations must be embedded, or `migrate up` cannot work from the
// distroless image: it has no filesystem to read .sql from and no shell to
// copy them in with.
func TestMigrationsAreEmbedded(t *testing.T) {
	t.Parallel()

	// Reading status only needs the embedded source, not the database, up to
	// the point it connects — so a missing embed fails before any container is
	// needed. Running against a fresh database proves the whole path.
	dsn := testenv.FreshDSN(t)
	st, err := postgres.MigrateStatus(dsn)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Pending == 0 {
		t.Fatal("no migrations were found; the embed is empty")
	}
}

// Several replicas start at once in compose and in Kubernetes. golang-migrate
// holds a Postgres advisory lock for the duration, so they cannot race each
// other through the same migration.
func TestConcurrentMigrationsAreSafe(t *testing.T) {
	t.Parallel()
	dsn := testenv.FreshDSN(t)

	const racers = 4
	errs := make(chan error, racers)
	start := make(chan struct{})

	for range racers {
		go func() {
			<-start
			errs <- postgres.MigrateUp(dsn)
		}()
	}
	close(start)

	for range racers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent migrate up: %v", err)
		}
	}

	st, err := postgres.MigrateStatus(dsn)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Dirty {
		t.Error("the schema is dirty after concurrent migrations")
	}
	if st.Pending != 0 {
		t.Errorf("pending = %d, want 0", st.Pending)
	}
}
