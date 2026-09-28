package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherMissingFileReportsNonClassifiedError(t *testing.T) {
	w := NewFileWatcher(filepath.Join(t.TempDir(), "absent.json"), 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	select {
	case err := <-w.Errs():
		t.Logf("missing-file error: %v", err)
		if _, ok := AsError(err); ok {
			t.Fatalf("I/O failure must not be reported as a classified config error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected an error for a missing config file")
	}
}

func TestLoadFileParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"version":"z","flags":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "z" {
		t.Fatalf("version=%q", cfg.Version)
	}
}
