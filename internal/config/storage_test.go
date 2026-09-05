package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// memory is the default because it is what makes `go run` work with no
// Postgres, no Docker and no configuration — the literal requirement.
func TestStorageDefaultsToMemory(t *testing.T) {
	t.Parallel()
	s := config.Defaults().Storage

	if s.Backend != config.BackendMemory {
		t.Errorf("backend = %q, want memory", s.Backend)
	}
	if s.Postgres.DSN != "" {
		t.Errorf("dsn = %q, want empty: a secret never has a built-in default", s.Postgres.DSN)
	}
	wantNoProblem(t, check(&s))
}

func TestStorageValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Storage)
		field  string
	}{
		"unknown backend": {
			func(s *config.Storage) { s.Backend = "sqlite" },
			"storage.backend",
		},
		"postgres needs a dsn": {
			func(s *config.Storage) { s.Backend = config.BackendPostgres },
			"storage.postgres.dsn",
		},
		"zero max conns": {
			func(s *config.Storage) {
				s.Backend = config.BackendPostgres
				s.Postgres.DSN = "postgres://taskapi@localhost/tasks"
				s.Postgres.MaxConns = 0
			},
			"storage.postgres.max_conns",
		},
		"min above max": {
			func(s *config.Storage) {
				s.Backend = config.BackendPostgres
				s.Postgres.DSN = "postgres://taskapi@localhost/tasks"
				s.Postgres.MinConns = s.Postgres.MaxConns + 1
			},
			"storage.postgres.min_conns",
		},
		"zero statement timeout": {
			func(s *config.Storage) {
				s.Backend = config.BackendPostgres
				s.Postgres.DSN = "postgres://taskapi@localhost/tasks"
				s.Postgres.StatementTimeout = 0
			},
			"storage.postgres.statement_timeout",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := config.Defaults().Storage
			tc.mutate(&s)
			wantProblem(t, check(&s), tc.field)
		})
	}

	t.Run("postgres with a dsn is fine", func(t *testing.T) {
		t.Parallel()
		s := config.Defaults().Storage
		s.Backend = config.BackendPostgres
		s.Postgres.DSN = "postgres://taskapi@localhost/tasks"
		wantNoProblem(t, check(&s))
	})
}

// A memory-backed run is not asked to justify a pool it will never build.
func TestStorageSkipsPoolRulesOnMemory(t *testing.T) {
	t.Parallel()
	s := config.Defaults().Storage
	s.Backend = config.BackendMemory
	s.Postgres.MaxConns = 0
	s.Postgres.MinConns = -5
	s.Postgres.StatementTimeout = 0

	wantNoProblem(t, check(&s))
}

// The DSN error names the environment variable, because that is where it
// belongs — never in the YAML file.
func TestStorageDSNErrorNamesTheEnvironmentVariable(t *testing.T) {
	t.Parallel()
	s := config.Defaults().Storage
	s.Backend = config.BackendPostgres

	wantProblem(t, check(&s), config.EnvName("storage.postgres.dsn"))
}
