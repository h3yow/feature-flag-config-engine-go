package reload

import (
	"context"

	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
	"github.com/h3yow/feature-flag-config-engine-go/internal/engine"
)

// Event reports one reload outcome.
type Event struct {
	Kind    string // "updated" (applied) or "rejected" (invalid)
	Version string
	Err     error
}

// Manager binds a FileWatcher to an Engine: every parsed config is fed
// through Engine.Update (which re-validates and applies atomically or
// rejects wholesale), and outcomes are exposed on Events().
type Manager struct {
	eng   *engine.Engine
	watch *config.FileWatcher

	events chan Event
}

// NewManager constructs a Manager.
func NewManager(e *engine.Engine, w *config.FileWatcher) *Manager {
	return &Manager{
		eng:    e,
		watch:  w,
		events: make(chan Event, 32),
	}
}

// Events returns the reload event channel (non-blocking delivery).
func (m *Manager) Events() <-chan Event { return m.events }

// Run forwards watcher configs/errors into the engine until ctx ends.
// It never blocks evaluation traffic: application work consists of one
// pointer swap, and events that cannot be queued are dropped (callers
// can always inspect engine state directly).
func (m *Manager) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-m.watch.Errs():
			if !ok {
				return nil
			}
			m.emit(Event{Kind: "rejected", Err: err})
		case cfg, ok := <-m.watch.Configs():
			if !ok {
				return nil
			}
			// Re-serialize is unnecessary: the watcher already parsed
			// it, but Engine.Update is the single validation/apply
			// choke point, so we feed it the raw document. To preserve
			// that choke point without the raw bytes here, the parsed
			// config is applied through a dedicated parsed-config path.
			m.applyParsed(cfg)
		}
	}
}

// applyParsed routes an already-parsed config into the engine. The
// engine guarantees the active version is untouched on rejection.
func (m *Manager) applyParsed(cfg *config.Config) {
	// The engine's ApplyConfig performs the same version-uniqueness
	// and atomicity checks as Update.
	if err := m.eng.ApplyConfig(cfg); err != nil {
		m.emit(Event{Kind: "rejected", Version: cfg.Version, Err: err})
		return
	}
	m.emit(Event{Kind: "updated", Version: cfg.Version})
}

func (m *Manager) emit(ev Event) {
	select {
	case m.events <- ev:
	default:
	}
}
