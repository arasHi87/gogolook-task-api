package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestAdminDefaults(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Admin

	if a.Addr != ":9090" {
		t.Errorf("addr = %q, want :9090", a.Addr)
	}
	if !a.Pprof {
		t.Error("pprof = false, want true: it is on the private listener, not the public one")
	}
	wantNoProblem(t, check(&a))
}

func TestAdminValidate(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Admin
	a.Addr = ""
	wantProblem(t, check(&a), "admin.addr")
}

// The rule that admin.addr must differ from http.addr cannot live here:
// neither section can see the other. It is asserted in config_test.go.
func TestAdminDoesNotKnowAboutTheHTTPListener(t *testing.T) {
	t.Parallel()
	a := config.Defaults().Admin
	a.Addr = config.Defaults().HTTP.Addr
	wantNoProblem(t, check(&a))
}
