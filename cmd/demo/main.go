// Command demo runs the feature flag engine against a local config
// file. It can evaluate a flag for a user once, watch the file for
// hot updates (without restarting the process), and roll back to an
// earlier version interactively.
//
// Examples:
//
//	# one-shot evaluation
//	demo -config configs/example.json -user u-42 -attr tier=gold -flag new_checkout
//
//	# hot-reload + rollback playground (stdin commands:
//	#   eval <flag>, versions, rollback <version>, quit)
//	demo -config configs/example.json -user u-42 -watch
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/config"
	"github.com/h3yow/feature-flag-config-engine-go/internal/engine"
	"github.com/h3yow/feature-flag-config-engine-go/internal/reload"
)

func main() {
	var (
		cfgPath  = flag.String("config", "configs/example.json", "path to the config file")
		userID   = flag.String("user", "u-42", "user id for evaluation")
		flagKey  = flag.String("flag", "", "flag key to evaluate once")
		watch    = flag.Bool("watch", false, "watch the config file and hot-reload changes")
		rollback = flag.String("rollback", "", "roll back to this version before evaluating")
		interval = flag.Duration("interval", 200*time.Millisecond, "poll interval when watching")
	)
	var attrs = map[string]string{}
	flag.Var(attrMap{attrs}, "attr", "user attribute key=value (repeatable)")
	flag.Parse()

	raw, err := config.ReadFile(*cfgPath)
	if err != nil {
		fatal("read config: %v", err)
	}
	eng, err := engine.New(raw)
	if err != nil {
		fatal("bootstrap rejected: %v", err)
	}
	defer eng.Close()

	printSnapshot("loaded", raw, eng)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *watch {
		w := config.NewFileWatcher(*cfgPath, *interval)
		mgr := reload.NewManager(eng, w)
		go func() { _ = w.Run(ctx) }()
		go func() { _ = mgr.Run(ctx) }()
		go func() {
			for ev := range mgr.Events() {
				if ev.Kind == "updated" {
					fmt.Printf("[reload] applied version=%s active=%s\n", ev.Version, eng.ActiveVersion())
				} else {
					fmt.Printf("[reload] REJECTED: %v\n", ev.Err)
				}
			}
		}()
	}

	user := engine.User{ID: *userID, Attributes: attrs}

	if *rollback != "" {
		if err := eng.Rollback(*rollback); err != nil {
			fatal("rollback: %v", err)
		}
		fmt.Printf("[rollback] now on version=%s fp=%s\n", eng.ActiveVersion(), eng.ActiveFingerprint())
	}

	if *flagKey != "" {
		printResult(eng, user, *flagKey)
	}

	if !*watch {
		return
	}

	// Interactive loop so rollback can be exercised without restart.
	fmt.Println("commands: eval <flag> | versions | rollback <version> | quit")
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("(v=%s)> ", eng.ActiveVersion())
		if !in.Scan() {
			return
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "quit", "exit":
			return
		case "versions":
			fmt.Println("versions (newest first):", strings.Join(eng.Versions(), ", "))
		case "eval":
			if len(parts) != 2 {
				fmt.Println("usage: eval <flag>")
				continue
			}
			printResult(eng, user, parts[1])
		case "rollback":
			if len(parts) != 2 {
				fmt.Println("usage: rollback <version>")
				continue
			}
			if err := eng.Rollback(parts[1]); err != nil {
				fmt.Printf("rollback rejected: %v\n", err)
				continue
			}
			fmt.Printf("rolled back to version=%s fp=%s\n", eng.ActiveVersion(), eng.ActiveFingerprint())
		default:
			fmt.Println("unknown command")
		}
	}
}

func printSnapshot(stage string, raw []byte, eng *engine.Engine) {
	fmt.Printf("[%s] version=%s fingerprint=%s\n", stage, eng.ActiveVersion(), eng.ActiveFingerprint())
	var compact map[string]any
	if err := json.Unmarshal(raw, &compact); err == nil {
		b, _ := json.Marshal(compact)
		fmt.Printf("[%s] config snapshot: %s\n", stage, b)
	}
}

func printResult(eng *engine.Engine, u engine.User, key string) {
	r := eng.Evaluate(u, key)
	b, _ := json.MarshalIndent(map[string]any{
		"user":             u,
		"flag":             key,
		"on":               r.On,
		"reason":           r.Reason,
		"bucket":           r.Bucket,
		"matched_audience": r.MatchedAud,
		"related_flag":     r.RelatedFlag,
	}, "", "  ")
	fmt.Println(string(b))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// attrMap adapts a map to flag.Value.
type attrMap struct{ m map[string]string }

func (a attrMap) String() string { b, _ := json.Marshal(a.m); return string(b) }
func (a attrMap) Set(s string) error {
	kv := strings.SplitN(s, "=", 2)
	if len(kv) != 2 || kv[0] == "" {
		return fmt.Errorf("invalid -attr %q, want key=value", s)
	}
	a.m[kv[0]] = kv[1]
	return nil
}
