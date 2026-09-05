package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// runReloader applies SIGHUP to the running process.
//
// The contract, and the reason it is worth having:
//
//   - Reload is all-or-nothing. The whole new configuration is parsed and
//     validated first; if anything is wrong, the running config is untouched
//     and we say so. A half-applied config is worse than a stale one.
//   - Only the hot set is applied. A SIGHUP that touches http.addr or the
//     storage backend logs a loud warning and skips those fields, rather than
//     pretending a listener moved.
//   - Every change gets its own log line naming the field, the old value and
//     the new one. "reload applied" with no detail is not operable.
func (a *App) runReloader(ctx context.Context) error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			a.reload()
		}
	}
}

// ReloadResult reports what a reload did, for the log line and for tests.
type ReloadResult struct {
	Applied     []config.Change
	RestartOnly []config.Change
	Err         error
}

// Reload re-reads the configuration and applies the hot set. It is exported so
// the admin listener and tests can trigger it without sending a signal.
func (a *App) Reload() ReloadResult { return a.reload() }

func (a *App) reload() ReloadResult {
	next, err := a.readConfig()
	if err != nil {
		return ReloadResult{Err: err}
	}

	hot, restartOnly, err := a.planReload(next)
	if err != nil {
		return ReloadResult{Err: err}
	}

	if len(hot) > 0 {
		if err := a.applyHot(next, hot); err != nil {
			return ReloadResult{Err: err}
		}
	}

	a.log.Info("SIGHUP reload applied", slog.Int("changed", len(hot)))
	a.signalReloaded()
	return ReloadResult{Applied: hot, RestartOnly: restartOnly}
}

// readConfig re-runs the same merge the process started with and checks the
// result. The flag set is replayed, not just the file: reloading from the file
// alone would drop every value that came from a flag, so a process started with
// -v would go quiet on its first SIGHUP.
func (a *App) readConfig() (*config.Config, error) {
	res, err := config.Load(config.Options{File: a.configFile, Flags: a.flags})
	if err != nil {
		a.reject("read error", err)
		return nil, err
	}
	if err := res.Config.Validate(); err != nil {
		a.reject("invalid", err)
		return nil, err
	}
	for _, w := range res.Warnings {
		a.log.Warn("config reload warning", slog.String("detail", w))
	}
	return res.Config, nil
}

// planReload works out what the new configuration would change, and reports the
// changes this process cannot make without a restart.
func (a *App) planReload(next *config.Config) (hot, restartOnly []config.Change, err error) {
	hot, restartOnly, err = config.Diff(a.Config(), next)
	if err != nil {
		a.reject("diff failed", err)
		return nil, nil, err
	}

	for _, c := range restartOnly {
		// Verbatim wording from our other services, so an operator who has seen
		// this line once recognises it here.
		a.log.Warn("load-time field change ignored",
			slog.String("field", c.Field), slog.String("from", c.From), slog.String("to", c.To))
	}
	return hot, restartOnly, nil
}

// applyHot swaps in the new configuration.
//
// The merge is validated before anything is published, so a reload is
// all-or-nothing: on any error the running configuration is untouched. A
// half-applied config is worse than a stale one.
func (a *App) applyHot(next *config.Config, hot []config.Change) error {
	merged, err := config.ApplyHot(a.Config(), next)
	if err != nil {
		a.reject("merge failed", err)
		return err
	}
	if err := merged.Validate(); err != nil {
		a.reject("invalid after merge", err)
		return err
	}

	a.mu.Lock()
	a.cfg = merged
	a.mu.Unlock()

	for _, c := range hot {
		a.log.Info("tunable changed",
			slog.String("field", c.Field), slog.String("from", c.From), slog.String("to", c.To))
	}

	// The log level is the one hot field that does not live in the config
	// struct at runtime: the handler holds it in a LevelVar the whole tree
	// shares, so it has to be moved explicitly.
	if err := a.log.SetLevel(merged.Logging.Level); err != nil {
		a.log.Warn("could not apply new log level", slog.Any("err", err))
	}
	return nil
}

// reject logs a refused reload. Every path that abandons a reload goes through
// here, so the running configuration being untouched is always said out loud.
func (a *App) reject(reason string, err error) {
	a.log.Error("config reload rejected",
		slog.String("reason", reason), slog.Any("err", err))
}

// signalReloaded is a non-blocking nudge for tests waiting on a reload.
func (a *App) signalReloaded() {
	select {
	case a.reloaded <- struct{}{}:
	default:
	}
}

// Reloaded returns a channel that receives after each completed reload.
func (a *App) Reloaded() <-chan struct{} { return a.reloaded }
