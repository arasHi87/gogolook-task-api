package config_test

import (
	"testing"
	"time"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

func TestQueueDefaults(t *testing.T) {
	t.Parallel()
	q := config.Defaults().Queue

	// lease/3: two consecutive missed heartbeats are tolerated before the
	// reaper acts, the same reasoning as a lease TTL versus a keepalive
	// interval in etcd or Raft.
	if q.HeartbeatInterval*3 != q.Lease {
		t.Errorf("heartbeat %v is not one third of lease %v", q.HeartbeatInterval, q.Lease)
	}
	if q.Backoff.Jitter != config.JitterFull {
		t.Errorf("jitter = %q, want full: equal and none both let replicas reconverge", q.Backoff.Jitter)
	}
	if q.MaxAttempts < 1 {
		t.Errorf("max_attempts = %d, want at least 1", q.MaxAttempts)
	}
	wantNoProblem(t, check(&q))
}

func TestQueueValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mutate func(*config.Queue)
		field  string
	}{
		"no workers":         {func(q *config.Queue) { q.Workers = 0 }, "queue.workers"},
		"no claim batch":     {func(q *config.Queue) { q.ClaimBatch = 0 }, "queue.claim_batch"},
		"no attempts":        {func(q *config.Queue) { q.MaxAttempts = 0 }, "queue.max_attempts"},
		"zero lease":         {func(q *config.Queue) { q.Lease = 0 }, "queue.lease"},
		"zero poll interval": {func(q *config.Queue) { q.PollInterval = 0 }, "queue.poll_interval"},
		"zero job timeout":   {func(q *config.Queue) { q.JobTimeout = 0 }, "queue.job_timeout"},
		"negative cooldown":  {func(q *config.Queue) { q.FetchCooldown = config.Duration(-time.Second) }, "queue.fetch_cooldown"},
		"backoff base above max": {
			func(q *config.Queue) { q.Backoff.Base = q.Backoff.Max + 1 },
			"queue.backoff.base",
		},
		"unknown jitter":      {func(q *config.Queue) { q.Backoff.Jitter = "sometimes" }, "queue.backoff.jitter"},
		"zero retention":      {func(q *config.Queue) { q.Retention.Succeeded = 0 }, "queue.retention.succeeded"},
		"zero purge interval": {func(q *config.Queue) { q.Retention.PurgeInterval = 0 }, "queue.retention.purge_interval"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := config.Defaults().Queue
			tc.mutate(&q)
			wantProblem(t, check(&q), tc.field)
		})
	}
}

// A heartbeat that is not comfortably inside the lease means one GC pause
// costs a worker the jobs it is actively running.
func TestQueueHeartbeatMustFitThriceInsideTheLease(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		heartbeat time.Duration
		lease     time.Duration
		ok        bool
	}{
		"exactly one third": {10 * time.Second, 30 * time.Second, true},
		"comfortably under": {5 * time.Second, 30 * time.Second, true},
		"a hair over":       {11 * time.Second, 30 * time.Second, false},
		"half the lease":    {15 * time.Second, 30 * time.Second, false},
		"equal":             {30 * time.Second, 30 * time.Second, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := config.Defaults().Queue
			q.HeartbeatInterval = config.Duration(tc.heartbeat)
			q.Lease = config.Duration(tc.lease)

			err := check(&q)
			if tc.ok {
				wantNoProblem(t, err)
			} else {
				wantProblem(t, err, "queue.heartbeat_interval")
			}
		})
	}
}
