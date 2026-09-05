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
	res, err := config.Load(config.Options{File: a.configFile, Flags: a.flags})
	if err != nil {
		a.log.Error("config reload rejected", slog.String("reason", "read error"), slog.Any("err", err))
		return ReloadResult{Err: err}
	}
	if err := res.Config.Validate(); err != nil {
		a.log.Error("config reload rejected", slog.String("reason", "invalid"), slog.Any("err", err))
		return ReloadResult{Err: err}
	}
	for _, w := range res.Warnings {
		a.log.Warn("config reload warning", slog.String("detail", w))
	}

	current := a.Config()
	hot, restartOnly, err := config.Diff(current, res.Config)
	if err != nil {
		a.log.Error("config reload rejected", slog.String("reason", "diff failed"), slog.Any("err", err))
		return ReloadResult{Err: err}
	}

	for _, c := range restartOnly {
		// Verbatim wording from our other services, so an operator who has seen
		// it once recognises it here.
		a.log.Warn("load-time field change ignored",
			slog.String("field", c.Field), slog.String("from", c.From), slog.String("to", c.To))
	}

	if len(hot) == 0 {
		a.log.Info("SIGHUP reload applied", slog.Int("changed", 0))
		a.signalReloaded()
		return ReloadResult{RestartOnly: restartOnly}
	}

	merged, err := config.ApplyHot(current, res.Config)
	if err != nil {
		a.log.Error("config reload rejected", slog.String("reason", "merge failed"), slog.Any("err", err))
		return ReloadResult{Err: err}
	}
	if err := merged.Validate(); err != nil {
		a.log.Error("config reload rejected", slog.String("reason", "invalid after merge"), slog.Any("err", err))
		return ReloadResult{Err: err}
	}

	a.mu.Lock()
	a.cfg = merged
	a.mu.Unlock()

	for _, c := range hot {
		a.log.Info("tunable changed",
			slog.String("field", c.Field), slog.String("from", c.From), slog.String("to", c.To))
	}

	// The log level is the one hot field that lives outside the config struct,
	// because the handler holds it in a LevelVar the whole tree shares.
	if err := a.log.SetLevel(merged.Logging.Level); err != nil {
		a.log.Warn("could not apply new log level", slog.Any("err", err))
	}

	a.log.Info("SIGHUP reload applied", slog.Int("changed", len(hot)))
	a.signalReloaded()
	return ReloadResult{Applied: hot, RestartOnly: restartOnly}
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
