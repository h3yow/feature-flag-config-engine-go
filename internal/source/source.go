package source

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/h3yow/feature-flag-config-engine-go/internal/errs"
)

// FileSource 从本地文件读取配置原文。
type FileSource struct {
	path string
}

func NewFileSource(path string) *FileSource { return &FileSource{path: path} }

func (f *FileSource) Path() string { return f.path }

func (f *FileSource) Read() ([]byte, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindState, "read_failed", f.path, "读取配置文件失败")
	}
	return raw, nil
}

// Watcher 轮询文件变化，内容真正变化时才调用 onChange。
// 回调由独立 goroutine 串行执行，回调返回错误时记录但不中断监听——
// 这样一份非法配置被整体拒绝后，修复文件即可再次生效，无需重启进程。
type Watcher struct {
	src           *FileSource
	onChange      func(raw []byte) error
	interval      time.Duration
	onError       func(error)
	lastBytes     []byte // 最后一次成功应用的内容
	lastSeenBytes []byte // 最后一次观测到的文件内容（无论应用成功与否），用于去重
}

type watchOpt func(*Watcher)

func NewWatcher(src *FileSource, onChange func([]byte) error, opts ...watchOpt) *Watcher {
	w := &Watcher{src: src, onChange: onChange, interval: time.Second}
	for _, o := range opts {
		o(w)
	}
	return w
}

// WithPollInterval 设置轮询间隔（默认 1s）。
func WithPollInterval(d time.Duration) watchOpt {
	return func(w *Watcher) { w.interval = d }
}

// WithErrorHandler 设置回调错误处理器（默认静默）。
func WithErrorHandler(h func(error)) watchOpt {
	return func(w *Watcher) { w.onError = h }
}

// Start 阻塞执行监听循环，直到 ctx 取消。
func (w *Watcher) Start(ctx context.Context) error {
	raw, err := w.src.Read()
	if err != nil {
		return err
	}
	w.lastBytes = raw
	w.lastSeenBytes = raw

	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.checkOnce(ctx)
		}
	}
}

// checkOnce 读取一次文件并在内容变化时回调。供 Start 循环与 ForceReload 复用。
//
// 为了避免读到“写文件写到一半”的截断态：
//   - 读取失败时保持上一版本观测，等下一轮重试；
//   - 回调判定内容非法（含截断 JSON）时同样不更新观测值，因此文件写完后
//     会以完整内容重新投递，不会把半截内容当成“最后观测版本”。
func (w *Watcher) checkOnce(ctx context.Context) bool {
	raw, err := w.src.Read()
	if err != nil {
		w.report(err)
		return false
	}
	if bytesEqual(raw, w.lastSeenBytes) {
		return false // 同一份内容（含同一份坏内容）不重复投递
	}
	// 先记下观测值再回调：坏内容不会被每个轮询周期重复投递；
	// 文件再次变化（如截断后写完整、修复后保存）时内容不同，自然会重新投递。
	w.lastSeenBytes = raw
	if err := w.onChange(raw); err != nil {
		w.report(err)
		return false
	}
	w.lastBytes = raw
	return true
}

// AtomicWrite 原子写文件：先写同目录临时文件再 rename，避免观察者读到截断态。
func AtomicWrite(path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cfg-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// ForceReload 立即重读文件并触发回调（即使内容未变，也会强制回调一次）。
func (w *Watcher) ForceReload() error {
	raw, err := w.src.Read()
	if err != nil {
		return err
	}
	if bytesEqual(raw, w.lastSeenBytes) {
		return nil
	}
	w.lastSeenBytes = raw
	if err := w.onChange(raw); err != nil {
		return err
	}
	w.lastBytes = raw
	return nil
}

func (w *Watcher) report(err error) {
	if w.onError != nil {
		w.onError(err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
