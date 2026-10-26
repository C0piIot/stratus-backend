package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/config"
)

// The settings that change while the server runs (#397), as the loops and the
// surfaces read them: once per pass or per request rather than once at
// startup, so a change from the web UI is in force on the next one.

// values are the settings in force: the service's when there is one, and the
// configuration's otherwise -- which is a test that built an App and its Deps
// by hand.
func (a *App) values(deps Deps) config.Values {
	if deps.Settings != nil {
		return deps.Settings.Values()
	}
	return a.cfg.Values
}

// changes is closed the next time a setting changes, and never for Deps with
// no service behind them. Ask for it before reading the values it guards: the
// other way round, a change landing between the two is one nobody hears.
func changes(deps Deps) <-chan struct{} {
	if deps.Settings == nil {
		return nil
	}
	return deps.Settings.Changed()
}

// every runs pass each time the interval the settings give for it goes by,
// until ctx ends or pass says to stop. A ticker and not a timer re-armed after
// each pass, for the reason each loop below gives; what a change does is look
// at the interval again, and start the clock afresh only when it moved. Zero
// is the pass switched off until a setting changes.
func (a *App) every(ctx context.Context, deps Deps, task string,
	interval func(config.Values) time.Duration, pass func() bool,
) {
	for {
		changed := changes(deps)
		d := interval(a.values(deps))
		if d <= 0 {
			slog.Info("off until a setting changes", "task", task)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		ticker := time.NewTicker(d)
		if !a.tick(ctx, deps, ticker, changed, d, interval, pass) {
			ticker.Stop()
			return
		}
		ticker.Stop()
	}
}

// tick is every's inner loop, over one ticker: false when the loop is over,
// true when the interval moved and the ticker has to be made again.
func (a *App) tick(ctx context.Context, deps Deps, ticker *time.Ticker, changed <-chan struct{},
	d time.Duration, interval func(config.Values) time.Duration, pass func() bool,
) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-changed:
			changed = changes(deps)
			if interval(a.values(deps)) != d {
				return true
			}
		case <-ticker.C:
			if !pass() {
				return false
			}
		}
	}
}

// followLogLevel keeps level at the setting in force, for as long as ctx
// lives. The handler reads the level on every record, so a change is in force
// on the next line.
func (a *App) followLogLevel(ctx context.Context, deps Deps) {
	for {
		changed := changes(deps)
		a.level.Set(a.values(deps).LogLevel)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}
