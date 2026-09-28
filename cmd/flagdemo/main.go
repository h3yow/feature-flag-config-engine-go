// 命令行 flagdemo：本地加载特性开关配置、判定、热更新与回滚的演示入口。
//
// 用法：
//
//	flagdemo load    <config.json>                 加载并打印版本信息
//	flagdemo versions <config.json>               列出历史版本（首次加载 + 演示一次更新）
//	flagdemo eval    <config.json> <flag> <uid> [k=v ...]  单次判定，打印判定依据
//	flagdemo rollback <config.json> <revision>    回滚到指定修订号并打印
//	flagdemo serve   <config.json> [--addr :8080] [--poll 1s]
//	                                               常驻进程：轮询热更新 + HTTP 判定
//	                                               SIGHUP 强制重载，SIGUSR1 回滚到上一版本
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/engine"
	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
	"github.com/h3yow/feature-flag-config-engine-go/internal/source"
	"github.com/h3yow/feature-flag-config-engine-go/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "load":
		err = cmdLoad(args)
	case "eval":
		err = cmdEval(args)
	case "versions":
		err = cmdVersions(args)
	case "rollback":
		err = cmdRollback(args)
	case "serve":
		err = cmdServe(args)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", formatErr(err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.TrimSpace(`
flagdemo - 本地配置与特性开关系统演示

用法:
  flagdemo load <config.json>
  flagdemo eval <config.json> <flag> <uid> [k=v ...]
  flagdemo versions <config.json>
  flagdemo rollback <config.json> <revision>
  flagdemo serve <config.json> [--addr :8080] [--poll 1s]
`))
}

func mustLoad(st *store.Store, path string) error {
	src := source.NewFileSource(path)
	raw, err := src.Read()
	if err != nil {
		return err
	}
	if _, err := st.PublishRaw(raw); err != nil {
		return err
	}
	return nil
}

func cmdLoad(args []string) error {
	if len(args) != 1 {
		return errors.New("load 需要 1 个参数：<config.json>")
	}
	st := store.New()
	if err := mustLoad(st, args[0]); err != nil {
		return err
	}
	snap := st.Current()
	fmt.Printf("加载成功 revision=%d version=%q sha=%s flags=%d\n",
		snap.Revision, snap.Config.Version, snap.ContentSHA[:12], len(snap.Config.Flags))
	return nil
}

func cmdEval(args []string) error {
	if len(args) < 3 {
		return errors.New("eval 至少需要 3 个参数：<config.json> <flag> <uid> [k=v ...]")
	}
	st := store.New()
	if err := mustLoad(st, args[0]); err != nil {
		return err
	}
	u := engine.User{ID: args[2], Attrs: map[string]string{}}
	for _, kv := range args[3:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("属性参数格式错误：%q，应为 k=v", kv)
		}
		u.Attrs[k] = v
	}
	d, err := engine.New(st).Eval(args[1], u)
	for _, t := range d.Trace {
		fmt.Println("  trace:", t)
	}
	if err != nil {
		return err
	}
	fmt.Printf("flag=%q user=%q revision=%d => on=%v reason=%s bucket=%d percentage=%v\n",
		d.FlagKey, u.ID, d.Revision, d.On, d.Reason, d.Bucket, d.Percentage)
	return nil
}

func cmdVersions(args []string) error {
	if len(args) != 1 {
		return errors.New("versions 需要 1 个参数：<config.json>")
	}
	st := store.New()
	if err := mustLoad(st, args[0]); err != nil {
		return err
	}
	printVersions(st)
	return nil
}

func cmdRollback(args []string) error {
	if len(args) != 2 {
		return errors.New("rollback 需要 2 个参数：<config.json> <revision>")
	}
	rev, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return errs.New(errs.KindInvalidValue, "bad_revision", "", "revision 必须是整数")
	}
	st := store.New()
	if err := mustLoad(st, args[0]); err != nil {
		return err
	}
	snap, err := st.Rollback(rev)
	if err != nil {
		return err
	}
	fmt.Printf("已回滚到 revision=%d version=%q\n", snap.Revision, snap.Config.Version)
	printVersions(st)
	return nil
}

func printVersions(st *store.Store) {
	for _, vi := range st.Versions() {
		fmt.Printf("  revision=%d version=%-10q sha=%s\n", vi.Revision, vi.Version, vi.ContentSHA[:12])
	}
}

func cmdServe(args []string) error {
	if len(args) < 1 {
		return errors.New("serve 需要参数：<config.json> [--addr :8080] [--poll 1s]")
	}
	path := args[0]
	addr := ":8080"
	poll := time.Second
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			i++
			addr = args[i]
		case "--poll":
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("非法 --poll：%w", err)
			}
			poll = d
		default:
			return fmt.Errorf("未知参数：%s", args[i])
		}
	}

	st := store.New()
	if err := mustLoad(st, path); err != nil {
		return err
	}
	eng := engine.New(st)
	src := source.NewFileSource(path)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	onChange := func(raw []byte) error {
		snap, err := st.PublishRaw(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[reload] 拒绝新版本：", formatErr(err))
			return err
		}
		fmt.Fprintf(os.Stderr, "[reload] 已发布 revision=%d version=%q\n", snap.Revision, snap.Config.Version)
		return nil
	}
	w := source.NewWatcher(src, onChange,
		source.WithPollInterval(poll),
		source.WithErrorHandler(func(err error) {
			fmt.Fprintln(os.Stderr, "[reload] 错误：", formatErr(err))
		}))

	// SIGHUP 强制重读；SIGUSR1 回滚到上一历史版本。
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				switch sig {
				case syscall.SIGHUP:
					if err := w.ForceReload(); err != nil {
						fmt.Fprintln(os.Stderr, "[sighup] 失败：", formatErr(err))
					}
				case syscall.SIGUSR1:
					if err := rollbackPrev(st); err != nil {
						fmt.Fprintln(os.Stderr, "[sigusr1] 回滚失败：", formatErr(err))
					}
				}
			}
		}
	}()

	go func() {
		if err := w.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "[watcher] 退出：", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/eval", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		u := engine.User{ID: q.Get("user"), Attrs: map[string]string{}}
		for k, vs := range q {
			if k == "user" || k == "flag" {
				continue
			}
			if len(vs) > 0 {
				u.Attrs[k] = vs[0]
			}
		}
		d, err := eng.Eval(q.Get("flag"), u)
		rw.Header().Set("Content-Type", "application/json")
		if err != nil {
			rw.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"error": formatErr(err), "decision": d,
			})
			return
		}
		_ = json.NewEncoder(rw).Encode(d)
	})
	mux.HandleFunc("/versions", func(rw http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(rw).Encode(st.Versions())
	})
	mux.HandleFunc("/rollback", func(rw http.ResponseWriter, r *http.Request) {
		rev, _ := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
		snap, err := st.Rollback(rev)
		rw.Header().Set("Content-Type", "application/json")
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(rw).Encode(map[string]string{"error": formatErr(err)})
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"revision": snap.Revision, "version": snap.Config.Version,
		})
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		fmt.Fprintf(os.Stderr, "[serve] 监听 %s （GET /eval?flag=k&user=u&attr=v），轮询 %s\n", addr, poll)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	<-ctx.Done()
	shCtx, shCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	return nil
}

func rollbackPrev(st *store.Store) error {
	vs := st.Versions()
	if len(vs) < 2 {
		return errs.New(errs.KindState, "no_previous_revision", "", "没有更早的版本可回滚")
	}
	target := vs[len(vs)-2]
	snap, err := st.Rollback(target.Revision)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[sigusr1] 已回滚到 revision=%d version=%q\n", snap.Revision, snap.Config.Version)
	return nil
}

// formatErr 把分类错误展开成 分类/码/路径/描述 形式。
func formatErr(err error) string {
	if err == nil {
		return ""
	}
	var se interface{ Unwrap() []error }
	if errors.As(err, &se) {
		var parts []string
		for _, e := range se.Unwrap() {
			parts = append(parts, "["+formatOne(e)+"]")
		}
		return strings.Join(parts, " ; ")
	}
	return formatOne(err)
}

func formatOne(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		loc := e.Path
		if loc == "" {
			loc = "-"
		}
		return fmt.Sprintf("kind=%s code=%s path=%s msg=%s", e.Kind, e.Code, loc, e.Msg)
	}
	return err.Error()
}
