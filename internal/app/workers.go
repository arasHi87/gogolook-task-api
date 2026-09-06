package app

import (
	"context"
)

// workers is the drain-order table. Read top to bottom, it is also the shutdown
// sequence: stop accepting new work, finish what was accepted, then let the
// maintenance loops go.
//
// Everything that runs for longer than one request appears here, and nothing
// appears anywhere else.
func (a *App) workers() []Worker {
	var ws []Worker

	// Drain order, top to bottom, and each position is a decision:
	//
	//   readiness       first, so a load balancer stops routing here before
	//                   the listener stops accepting. The requests that arrive
	//                   in that window are still served.
	//   api             stop accepting, finish what was accepted.
	//   queue-metrics   drains the pool's completion events. After the pool,
	//                   so the last job's outcome is still recorded.
	//   idempotency-purge
	//   ratelimit-sweeper
	//                   housekeeping loops; nothing waits on them.
	//   config-reloader a reload racing a shutdown helps nobody.
	//   admin           near last, so health and metrics answer for the whole
	//                   drain rather than going dark at the start of it.
	//   tracing         last of all, flushing the spans every worker above it
	//                   produced on its way out.
	ws = append(ws, Worker{
		Name: "readiness",
		Run:  waitForShutdown,
		Stop: func(context.Context) error {
			a.health.StartDraining()
			a.log.Info("readiness flipped to not-ready, draining")
			return nil
		},
	})

	if a.apiServer != nil {
		ws = append(ws, serveWorker("api", a.apiServer, a.log.Logger))
	}

	// The queue drains after the API: a job enqueued by the last request
	// accepted should still be picked up, and Stop hands back anything that
	// does not finish in time rather than letting its lease expire.
	if a.jobs != nil {
		ws = append(ws,
			Worker{Name: "queue-listener", Run: a.listener.Run},
			Worker{Name: "queue-workers", Run: a.jobs.Run, Stop: a.jobs.Stop},
			Worker{Name: "queue-maintenance", Run: a.maintenance.Run},
		)
	}

	// Leader-elected, so running it in every mode costs nothing and means the
	// keys keep being purged even when only workers are up.
	if a.keysPurge != nil {
		ws = append(ws, Worker{Name: "idempotency-purge", Run: a.keysPurge.Run})
	}

	// The queue's own metrics come from its completion events, which is the
	// only place the wait and the processing time are both known.
	if a.jobs != nil {
		ws = append(ws, Worker{Name: "queue-metrics", Run: a.recordJobOutcomes})
	}

	// The limiter's sweeper evicts idle buckets. It runs wherever the limiter
	// does, which is wherever there is a public listener.
	if a.limiter != nil && a.apiServer != nil {
		ws = append(ws, Worker{Name: "ratelimit-sweeper", Run: a.limiter.Run})
	}

	ws = append(ws, Worker{Name: "config-reloader", Run: a.runReloader})
	ws = append(ws, serveWorker("admin", a.adminServer, a.log.Logger))

	// Last, and it has to be: the exporter batches for up to five seconds, so
	// a process that exits without flushing loses the spans from whatever
	// caused the restart — which is the one trace anybody wanted.
	ws = append(ws, Worker{
		Name: "tracing",
		Run:  waitForShutdown,
		Stop: a.tracing.Shutdown,
	})

	return ws
}

// waitForShutdown is the Run of a worker whose whole job is its Stop.
func waitForShutdown(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
