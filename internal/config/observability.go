package config

// Observability configures the metrics registry and the tracing exporter.
type Observability struct {
	Metrics Metrics `koanf:"metrics" yaml:"metrics" json:"metrics"`
	Tracing Tracing `koanf:"tracing" yaml:"tracing" json:"tracing"`
}

// Metrics toggles the Prometheus surface.
type Metrics struct {
	Enabled bool `koanf:"enabled" yaml:"enabled" json:"enabled"`
	// NativeHistograms exposes native histograms alongside the classic buckets.
	// They cost nothing to emit and are what an OTel-native backend prefers.
	NativeHistograms bool `koanf:"native_histograms" yaml:"native_histograms" json:"native_histograms"`
}

// Tracing configures OTLP export.
type Tracing struct {
	Enabled      bool    `koanf:"enabled"       yaml:"enabled"       json:"enabled"`
	OTLPEndpoint string  `koanf:"otlp_endpoint" yaml:"otlp_endpoint" json:"otlp_endpoint"`
	SampleRatio  float64 `koanf:"sample_ratio"  yaml:"sample_ratio"  json:"sample_ratio"`
}

// Path implements Section.
func (Observability) Path() string { return "observability" }

// SetDefaults implements Section. Tracing is off because it needs a collector
// to send to; metrics are on because they need nothing.
func (o *Observability) SetDefaults() {
	*o = Observability{
		Metrics: Metrics{Enabled: true, NativeHistograms: true},
		Tracing: Tracing{Enabled: false, OTLPEndpoint: "", SampleRatio: 0.1},
	}
}

// Validate implements Section.
func (o *Observability) Validate(p *Problems) {
	at := func(f string) string { return join(o.Path(), f) }

	if o.Tracing.Enabled && o.Tracing.OTLPEndpoint == "" {
		p.Add(at("tracing.otlp_endpoint"), "required when tracing is enabled")
	}
	if r := o.Tracing.SampleRatio; r < 0 || r > 1 {
		p.Add(at("tracing.sample_ratio"), "must be between 0 and 1")
	}
}
