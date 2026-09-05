package config

// Admin is the private listener: metrics, health, pprof, debug endpoints.
//
// It is a separate port so none of that is ever reachable from the public one.
// The rule that the two addresses must differ is enforced in
// Config.validateAcrossSections, since neither section can see the other.
type Admin struct {
	Addr  string `koanf:"addr"  yaml:"addr"  json:"addr"`
	Pprof bool   `koanf:"pprof" yaml:"pprof" json:"pprof"`
}

// Path implements Section.
func (Admin) Path() string { return "admin" }

// SetDefaults implements Section.
func (a *Admin) SetDefaults() {
	*a = Admin{
		Addr:  ":9090",
		Pprof: true,
	}
}

// Validate implements Section.
func (a *Admin) Validate(p *Problems) {
	p.NotEmpty(join(a.Path(), "addr"), a.Addr)
}
