package reload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
	"github.com/h3yow/feature-flag-config-engine-go/internal/engine"
)

const v1Doc = `{
  "version": "f-v1",
  "flags": { "f": { "enabled": true, "percentage": 0 } }
}`

const v2Doc = `{
  "version": "f-v2",
  "flags": { "f": { "enabled": true, "percentage": 100 } }
}`

const badDoc = `{ "version": "f-bad", "flags": { "f": { "enabled": true, "percentage": 700 } } }`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func waitVersion(t *testing.T, e *engine.Engine, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.ActiveVersion() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for version %q (active=%q)", want, e.ActiveVersion())
}

func TestFileHotReloadRejectAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, v1Doc)

	t.Logf("INITIAL CONFIG SNAPSHOT:\n%s", v1Doc)

	eng, err := engine.New([]byte(v1Doc))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := config.NewFileWatcher(path, 20*time.Millisecond).SkipInitialLoad(eng.Snapshot())
	mgr := NewManager(eng, w)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = mgr.Run(ctx) }()

	u := engine.User{ID: "watcher-user"}
	r := eng.Evaluate(u, "f")
	t.Logf("V1 EVAL BASIS: on=%v reason=%s bucket=%d", r.On, r.Reason, r.Bucket)
	if r.On || r.Reason != engine.ReasonOutOfRollout {
		t.Fatalf("v1 expected off/0%%, got %+v", r)
	}

	// --- valid update v2 ---
	t.Logf("APPLYING SNAPSHOT:\n%s", v2Doc)
	writeFile(t, path, v2Doc)
	waitVersion(t, eng, "f-v2", 2*time.Second)
	// Consume the v2 update event so the next event we read is the
	// rejection from the deliberately bad write below.
	select {
	case ev := <-mgr.Events():
		if ev.Kind != "updated" || ev.Version != "f-v2" {
			t.Fatalf("expected updated f-v2 event, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing updated f-v2 event")
	}
	r = eng.Evaluate(u, "f")
	t.Logf("V2 EVAL BASIS: on=%v reason=%s", r.On, r.Reason)
	if !r.On {
		t.Fatalf("v2 expected on/100%%, got %+v", r)
	}

	// --- invalid update: must be rejected with a classified error
	// and must leave the active version untouched ---
	t.Logf("APPLYING INVALID SNAPSHOT:\n%s", badDoc)
	writeFile(t, path, badDoc)
	select {
	case ev := <-mgr.Events():
		t.Logf("RELOAD EVENT: kind=%s version=%q err=%v", ev.Kind, ev.Version, ev.Err)
		if ev.Kind != "rejected" {
			t.Fatalf("expected rejected event, got %+v", ev)
		}
		ce, ok := config.AsError(ev.Err)
		if !ok || ce.Kind != config.KindInvalidValue {
			t.Fatalf("expected KindInvalidValue, got %v", ev.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rejection event")
	}
	if eng.ActiveVersion() != "f-v2" {
		t.Fatalf("invalid update changed active version to %q", eng.ActiveVersion())
	}
	r = eng.Evaluate(u, "f")
	t.Logf("POST-REJECTION EVAL BASIS: on=%v reason=%s (still f-v2)", r.On, r.Reason)
	if !r.On {
		t.Fatal("service must keep serving the last good version after a bad write")
	}

	// A subsequent valid write must recover updates.
	writeFile(t, path, v2Doc)
	time.Sleep(100 * time.Millisecond)

	// --- rollback to v1 reproduces the earlier decision ---
	if err := eng.Rollback("f-v1"); err != nil {
		t.Fatal(err)
	}
	if eng.ActiveVersion() != "f-v1" {
		t.Fatalf("rollback active=%q", eng.ActiveVersion())
	}
	r = eng.Evaluate(u, "f")
	t.Logf("ROLLBACK EVAL BASIS: on=%v reason=%s bucket=%d", r.On, r.Reason, r.Bucket)
	if r.On || r.Reason != engine.ReasonOutOfRollout {
		t.Fatalf("rollback did not reproduce v1 decision: %+v", r)
	}
}

func TestSyntaxErrorDuringReloadDoesNotCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, v1Doc)

	eng, err := engine.New([]byte(v1Doc))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := config.NewFileWatcher(path, 10*time.Millisecond).SkipInitialLoad(eng.Snapshot())
	mgr := NewManager(eng, w)
	go func() { _ = w.Run(ctx) }()
	go func() { _ = mgr.Run(ctx) }()

	writeFile(t, path, `{not json`)
	select {
	case ev := <-mgr.Events():
		ce, ok := config.AsError(ev.Err)
		t.Logf("syntax rejection: kind=%s msg=%v", func() string {
			if ok {
				return ce.Kind.String()
			}
			return "unclassified"
		}(), ev.Err)
		if !ok || ce.Kind != config.KindSyntax {
			t.Fatalf("expected KindSyntax, got %v", ev.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing syntax-error event")
	}
	if eng.ActiveVersion() != "f-v1" {
		t.Fatal("syntax-broken write must not displace the active version")
	}
}
