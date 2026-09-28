package engine

import (
	"sync"
	"sync/atomic"

	"github.com/h3yow/feature-flag-config-engine-go/internal/bucket"
	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
)

// User is re-exported so callers only depend on the engine package.
type User = bucket.User

// maxHistory bounds how many previous versions are retained for
// rollback. Activating the same content twice never doubles the list.
const maxHistory = 20

// snapshot is one immutable, fully validated configuration version.
type snapshot struct {
	cfg *config.Config
}

// Engine holds an atomic pointer to the active snapshot and a bounded
// history of previously activated versions for rollback.
//
// Reads (Evaluate/Snapshot) are completely lock-free: they load one
// atomic pointer and then operate on an immutable snapshot. Writers
// (Update/Rollback) take a mutex only to mutate the small history slice
// and swap the pointer, so frequent updates can never block evaluations
// mid-flight, and no reader can ever observe a half-built config.
type Engine struct {
	active  atomic.Pointer[snapshot]
	mu      sync.Mutex
	history []*snapshot // newest (== active) first, unique fingerprints
}

// New builds an engine. bootstrap may be nil/empty: the engine starts
// with no active config and every flag evaluates OFF
// (ReasonUnknownFlag) until the first successful Update.
func New(bootstrap []byte) (*Engine, error) {
	e := &Engine{}
	if len(bootstrap) > 0 {
		cfg, err := config.Parse(bootstrap)
		if err != nil {
			return nil, err
		}
		e.activateLocked(&snapshot{cfg: cfg})
	}
	return e, nil
}

// Update atomically validates and activates a new raw configuration.
// An invalid document is rejected as a classified *config.Error and the
// active version stays byte-for-byte untouched: updates are all or
// nothing. Reusing a known version label with different content is also
// rejected, so labels remain trustworthy rollback targets.
func (e *Engine) Update(raw []byte) error {
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	return e.ApplyConfig(cfg)
}

// ApplyConfig applies an already-parsed config. It is the single choke
// point used by both Update and the hot-reload manager, so version
// uniqueness checks and the atomic swap cannot diverge between paths.
func (e *Engine) ApplyConfig(cfg *config.Config) error {
	if cfg == nil {
		return &config.Error{Kind: config.KindInvalidValue, Path: "(root)", Message: "nil config"}
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if cur := e.currentCfgLocked(); cur != nil {
		if cfg.Version == cur.Version && cfg.Fingerprint != cur.Fingerprint {
			return &config.Error{
				Kind: config.KindConflict,
				Path: "version",
				Message: "version label \"" + cfg.Version +
					"\" is already used by different content; labels must be unique",
			}
		}
		if cfg.Fingerprint == cur.Fingerprint {
			return nil // identical content: no-op
		}
	}
	for _, h := range e.history {
		if h.cfg.Version == cfg.Version && h.cfg.Fingerprint != cfg.Fingerprint {
			return &config.Error{
				Kind:    config.KindConflict,
				Path:    "version",
				Message: "version label \"" + cfg.Version + "\" was previously used by different content",
			}
		}
	}

	e.activateLocked(&snapshot{cfg: cfg})
	return nil
}

// Rollback atomically switches to a previously activated version label.
// Because the snapshot is immutable, evaluations after rollback are
// identical to evaluations when that version was active.
func (e *Engine) Rollback(version string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, h := range e.history {
		if h.cfg.Version == version {
			e.activateLocked(h)
			return nil
		}
	}
	return &config.Error{
		Kind:    config.KindReference,
		Path:    "version",
		Message: "no retained version with label \"" + version + "\"",
	}
}

// RollbackToFingerprint switches back to the version whose content
// fingerprint matches. This guarantees decision identity with the
// earlier moment even if callers are unsure about labels.
func (e *Engine) RollbackToFingerprint(fp string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, h := range e.history {
		if h.cfg.Fingerprint == fp {
			e.activateLocked(h)
			return nil
		}
	}
	return &config.Error{
		Kind:    config.KindReference,
		Path:    "fingerprint",
		Message: "no retained version with fingerprint " + fp,
	}
}

func (e *Engine) activateLocked(s *snapshot) {
	// History keeps unique fingerprints; re-activation moves the
	// snapshot to the front (it becomes the newest/active one).
	kept := e.history[:0]
	for _, h := range e.history {
		if h.cfg.Fingerprint != s.cfg.Fingerprint {
			kept = append(kept, h)
		}
	}
	e.history = append([]*snapshot{s}, kept...)
	if len(e.history) > maxHistory {
		e.history = e.history[:maxHistory]
	}
	// Single atomic publication: readers either see the old world or
	// the new world, never anything in between.
	e.active.Store(s)
}

func (e *Engine) currentCfgLocked() *config.Config {
	if s := e.active.Load(); s != nil {
		return s.cfg
	}
	return nil
}

// ActiveVersion returns the version label of the active snapshot ("" if none).
func (e *Engine) ActiveVersion() string {
	if s := e.active.Load(); s != nil {
		return s.cfg.Version
	}
	return ""
}

// ActiveFingerprint returns the content fingerprint of the active snapshot.
func (e *Engine) ActiveFingerprint() string {
	if s := e.active.Load(); s != nil {
		return s.cfg.Fingerprint
	}
	return ""
}

// Versions returns retained version labels, newest/active first.
func (e *Engine) Versions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.history))
	for _, h := range e.history {
		out = append(out, h.cfg.Version)
	}
	return out
}

// Snapshot returns the currently active immutable config (may be nil).
func (e *Engine) Snapshot() *config.Config {
	if s := e.active.Load(); s != nil {
		return s.cfg
	}
	return nil
}

// Close is currently a no-op but lets callers treat Engine as a
// lifecycle resource uniformly.
func (e *Engine) Close() {}
