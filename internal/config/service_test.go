package config_test

import (
	"os"
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestServiceDefaults(t *testing.T) {
	t.Parallel()
	s := config.Defaults().Service

	if s.Name != "taskapi" {
		t.Errorf("name = %q, want taskapi", s.Name)
	}
	// The instance names this replica in application_name, in the queue's
	// locked_by column and in every log record, which is what makes "which
	// replica held that lease" answerable.
	host, _ := os.Hostname()
	if s.Instance == "" || (host != "" && s.Instance != host) {
		t.Errorf("instance = %q, want the hostname %q", s.Instance, host)
	}
	wantNoProblem(t, check(&s))
}

func TestServiceValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Service)
		field  string
	}{
		"empty name":     {func(s *config.Service) { s.Name = "" }, "service.name"},
		"blank name":     {func(s *config.Service) { s.Name = "   " }, "service.name"},
		"empty instance": {func(s *config.Service) { s.Instance = "" }, "service.instance"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := config.Defaults().Service
			tc.mutate(&s)
			wantProblem(t, check(&s), tc.field)
		})
	}
}

func TestServicePath(t *testing.T) {
	t.Parallel()
	var s config.Service
	if got := s.Path(); got != "service" {
		t.Errorf("Path() = %q, want service", got)
	}
}
