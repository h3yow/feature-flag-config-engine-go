package config

import (
	"context"
	"os"
	"sync"
	"time"
)

// ReadFile reads the raw bytes of a configuration file.
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// LoadFile reads and parses a configuration file from disk.
func LoadFile(path string) (*Config, error) {
	data, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// FileWatcher polls a local configuration file and delivers parsed
// configs on Configs(); invalid updates are delivered on Errs() and
// never replace the active configuration. Polling (rather than fs
// events) keeps the engine dependency-free and means a half-written
// file simply surfaces as a classified error that the next complete
// write overwrites.
type FileWatcher struct {
	path     string
	interval time.Duration

	configs chan *Config
	errs    chan error

	skipFirst bool

	mu       sync.Mutex
	baseline bool // a baseline fingerprint has been established
	lastFP   string
	lastStat os.FileInfo
}

// NewFileWatcher constructs a FileWatcher polling path at interval.
func NewFileWatcher(path string, interval time.Duration) *FileWatcher {
	if interval <= 0 {
		interval = time.Second
	}
	return &FileWatcher{
		path:     path,
		interval: interval,
		configs:  make(chan *Config, 16),
		errs:     make(chan error, 16),
	}
}

// SkipInitialLoad configures the watcher to treat the file's first
// successful read as a baseline (cfg seeds that baseline, typically the
// config the process bootstrapped with) and emit only subsequent
// changes. Must be called before Run.
func (w *FileWatcher) SkipInitialLoad(cfg *Config) *FileWatcher {
	w.skipFirst = true
	if cfg != nil {
		w.lastFP = cfg.Fingerprint
		w.baseline = true
	}
	return w
}

// Configs returns the channel of successfully parsed configurations.
func (w *FileWatcher) Configs() <-chan *Config { return w.configs }

// Errs returns the channel of rejected-update errors.
func (w *FileWatcher) Errs() <-chan error { return w.errs }

// Run polls until the context is cancelled. It performs one immediate
// check at startup so an initial config is delivered without waiting
// for the first tick.
func (w *FileWatcher) Run(ctx context.Context) error {
	w.checkOnce()
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.checkOnce()
		}
	}
}

func (w *FileWatcher) checkOnce() {
	fi, err := os.Stat(w.path)
	if err != nil {
		// A missing/unreadable file is an I/O problem, not a problem
		// with configuration content, so report the raw os error
		// (callers see it is not *config.Error).
		w.reportErr(err)
		return
	}
	w.mu.Lock()
	unchanged := w.baseline && sameStat(fi, w.lastStat)
	w.mu.Unlock()
	if unchanged {
		return
	}

	data, err := ReadFile(w.path)
	if err != nil {
		w.reportErr(err)
		return
	}
	cfg, err := Parse(data)
	if err != nil {
		w.reportErr(err)
		return
	}

	emit := true
	w.mu.Lock()
	switch {
	case w.skipFirst && !w.baseline:
		// First successful read: establish baseline, emit nothing.
		emit = false
	case w.baseline && cfg.Fingerprint == w.lastFP:
		// Same content (e.g. touched file, reused labels): no event.
		emit = false
	}
	w.baseline = true
	w.lastFP = cfg.Fingerprint
	w.lastStat = fi
	w.mu.Unlock()

	if emit {
		select {
		case w.configs <- cfg:
		default:
			// Drop if nobody consumed the previous event yet; the
			// next change will deliver the newest version. Events are
			// advisory; the poll loop never blocks on a slow consumer.
		}
	}
}

func sameStat(a, b os.FileInfo) bool {
	if b == nil {
		return false
	}
	return a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func (w *FileWatcher) reportErr(err error) {
	select {
	case w.errs <- err:
	default:
	}
}
