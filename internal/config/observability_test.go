package config_test

import (
	"testing"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// Metrics need nothing to be useful; tracing needs a collector to send to.
func TestObservabilityDefaults(t *testing.T) {
	t.Parallel()
	o := config.Defaults().Observability

	if !o.Metrics.Enabled {
		t.Error("metrics are off by default; they cost nothing and need no dependency")
	}
	if o.Tracing.Enabled {
		t.Error("tracing is on by default, but there is no collector to send to")
	}
	wantNoProblem(t, check(&o))
}

func TestObservabilityValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Observability)
		field  string
	}{
		"tracing without an endpoint": {
			func(o *config.Observability) { o.Tracing.Enabled = true },
			"observability.tracing.otlp_endpoint",
		},
		"sample ratio above one": {
			func(o *config.Observability) { o.Tracing.SampleRatio = 1.5 },
			"observability.tracing.sample_ratio",
		},
		"negative sample ratio": {
			func(o *config.Observability) { o.Tracing.SampleRatio = -0.1 },
			"observability.tracing.sample_ratio",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := config.Defaults().Observability
			tc.mutate(&o)
			wantProblem(t, check(&o), tc.field)
		})
	}

	t.Run("tracing with an endpoint is fine", func(t *testing.T) {
		t.Parallel()
		o := config.Defaults().Observability
		o.Tracing.Enabled = true
		o.Tracing.OTLPEndpoint = "otel-collector:4317"
		wantNoProblem(t, check(&o))
	})

	t.Run("the ratio bounds are inclusive", func(t *testing.T) {
		t.Parallel()
		for _, r := range []float64{0, 0.5, 1} {
			o := config.Defaults().Observability
			o.Tracing.SampleRatio = r
			wantNoProblem(t, check(&o))
		}
	})
}
