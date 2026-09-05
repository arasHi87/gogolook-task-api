package config

import "time"

// Retry backoff jitter strategies.
const (
	// JitterFull is sleep = rand(0, min(cap, base*2^n)). It is what AWS
	// recommends: equal jitter and no jitter both let replicas reconverge.
	JitterFull  = "full"
	JitterEqual = "equal"
	JitterNone  = "none"
)

// Queue is the Postgres-backed job queue.
type Queue struct {
	Workers    int `koanf:"workers"     yaml:"workers"     json:"workers"`
	ClaimBatch int `koanf:"claim_batch" yaml:"claim_batch" json:"claim_batch"`
	// Lease is the visibility timeout: how long a claim is honoured without a
	// heartbeat before the reaper may reclaim the job.
	Lease             Duration `koanf:"lease"              yaml:"lease"              json:"lease"`
	HeartbeatInterval Duration `koanf:"heartbeat_interval" yaml:"heartbeat_interval" json:"heartbeat_interval"`
	PollInterval      Duration `koanf:"poll_interval"      yaml:"poll_interval"      json:"poll_interval"`
	ReaperInterval    Duration `koanf:"reaper_interval"    yaml:"reaper_interval"    json:"reaper_interval"`
	// FetchCooldown is the minimum gap between claims after a NOTIFY, so a
	// thousand-row insert burst does not become a thousand claim round-trips.
	FetchCooldown Duration  `koanf:"fetch_cooldown" yaml:"fetch_cooldown" json:"fetch_cooldown"`
	JobTimeout    Duration  `koanf:"job_timeout"    yaml:"job_timeout"    json:"job_timeout"`
	MaxAttempts   int       `koanf:"max_attempts"   yaml:"max_attempts"   json:"max_attempts"`
	Backoff       Backoff   `koanf:"backoff"        yaml:"backoff"        json:"backoff"`
	Retention     Retention `koanf:"retention"      yaml:"retention"      json:"retention"`
}

// Backoff is the retry schedule for failed jobs.
type Backoff struct {
	Base   Duration `koanf:"base"   yaml:"base"   json:"base"`
	Max    Duration `koanf:"max"    yaml:"max"    json:"max"`
	Jitter string   `koanf:"jitter" yaml:"jitter" json:"jitter"`
}

// Retention governs the purge loop. A queue table that never deletes grows
// forever and takes its indexes with it.
type Retention struct {
	Succeeded     Duration `koanf:"succeeded"      yaml:"succeeded"      json:"succeeded"`
	Discarded     Duration `koanf:"discarded"      yaml:"discarded"      json:"discarded"`
	PurgeInterval Duration `koanf:"purge_interval" yaml:"purge_interval" json:"purge_interval"`
}

// Path implements Section.
func (Queue) Path() string { return "queue" }

// SetDefaults implements Section.
func (q *Queue) SetDefaults() {
	*q = Queue{
		Workers:    8,
		ClaimBatch: 10,
		Lease:      Duration(30 * time.Second),
		// lease/3: two consecutive missed heartbeats are tolerated before the
		// reaper acts, the same reasoning as a lease TTL versus a keepalive
		// interval in etcd or Raft.
		HeartbeatInterval: Duration(10 * time.Second),
		PollInterval:      Duration(2 * time.Second),
		ReaperInterval:    Duration(15 * time.Second),
		FetchCooldown:     Duration(100 * time.Millisecond),
		JobTimeout:        Duration(1 * time.Minute),
		MaxAttempts:       5,
		Backoff: Backoff{
			Base:   Duration(1 * time.Second),
			Max:    Duration(5 * time.Minute),
			Jitter: JitterFull,
		},
		Retention: Retention{
			Succeeded:     Duration(24 * time.Hour),
			Discarded:     Duration(7 * 24 * time.Hour),
			PurgeInterval: Duration(1 * time.Hour),
		},
	}
}

// Validate implements Section.
func (q *Queue) Validate(p *Problems) {
	at := func(f string) string { return join(q.Path(), f) }

	p.AtLeast(at("workers"), q.Workers, 1)
	p.AtLeast(at("claim_batch"), q.ClaimBatch, 1)
	p.AtLeast(at("max_attempts"), q.MaxAttempts, 1)

	p.Positive(at("lease"), q.Lease)
	p.Positive(at("heartbeat_interval"), q.HeartbeatInterval)
	p.Positive(at("poll_interval"), q.PollInterval)
	p.Positive(at("reaper_interval"), q.ReaperInterval)
	p.Positive(at("job_timeout"), q.JobTimeout)

	if q.FetchCooldown < 0 {
		p.Add(at("fetch_cooldown"), "must not be negative")
	}

	// Two missed heartbeats must still fit inside the lease, or a worker that
	// pauses for one GC cycle loses jobs it is actively running.
	if q.HeartbeatInterval > 0 && q.Lease > 0 && q.HeartbeatInterval*3 > q.Lease {
		p.Add(at("heartbeat_interval"),
			"must be at most one third of %s (%s), so two missed heartbeats are tolerated before the reaper acts",
			at("lease"), q.Lease)
	}

	q.Backoff.validate(p, at("backoff"))
	q.Retention.validate(p, at("retention"))
}

func (b *Backoff) validate(p *Problems, prefix string) {
	at := func(f string) string { return join(prefix, f) }

	p.Positive(at("base"), b.Base)
	p.Positive(at("max"), b.Max)
	if b.Max > 0 && b.Base > b.Max {
		p.Add(at("base"), "must not exceed %s", at("max"))
	}
	p.OneOf(at("jitter"), b.Jitter, JitterFull, JitterEqual, JitterNone)
}

func (r *Retention) validate(p *Problems, prefix string) {
	p.Positive(join(prefix, "succeeded"), r.Succeeded)
	p.Positive(join(prefix, "discarded"), r.Discarded)
	p.Positive(join(prefix, "purge_interval"), r.PurgeInterval)
}
