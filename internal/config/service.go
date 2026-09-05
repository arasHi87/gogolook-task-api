package config

import "os"

// Service identifies the process in logs, metrics and pg_stat_activity.
type Service struct {
	Name string `koanf:"name" yaml:"name" json:"name"`
	// Instance defaults to the hostname. It disambiguates replicas in
	// application_name and in the queue's locked_by column, which is what makes
	// "which replica was holding that lease" an answerable question.
	Instance string `koanf:"instance" yaml:"instance" json:"instance"`
}

// Path implements Section.
func (Service) Path() string { return "service" }

// SetDefaults implements Section.
func (s *Service) SetDefaults() {
	*s = Service{
		Name:     "taskapi",
		Instance: hostname(),
	}
}

// Validate implements Section.
func (s *Service) Validate(p *Problems) {
	p.NotEmpty(join(s.Path(), "name"), s.Name)
	p.NotEmpty(join(s.Path(), "instance"), s.Instance)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
