// Package diagnostics provides bounded, password-redacted process logs.
package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Log retention and password-only redaction: see docs/adr/0010-process-diagnostics-and-progress.md.
const (
	maxFileSize = 10 * 1024 * 1024
	backups     = 4
	// Unlike RFC3339's Z layout, keep the numeric offset even in UTC.
	timestampFormat = "2006-01-02T15:04:05-07:00"
)

// Sink owns the active log and retains its first write error for Close.
type Sink struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	root      *os.Root
	lock      *os.File
	done      chan struct{}
	ctx       context.Context
	closeOnce sync.Once
	size      int64
	limit     int64
	logger    *slog.Logger
	redactor  redactor
	err       error
	writeErr  error
	closed    bool
}

// New uses dir directly, or the platform user cache's imap-mcp directory when
// dir is empty. Debug stderr receives only DEBUG stage and duration events.
func New(dir, password string, debug bool, debugStderr io.Writer) (*Sink, error) {
	return NewContext(context.Background(), dir, password, debug, debugStderr)
}

// NewContext uses ctx to interrupt startup and logging waits, including records
// whose request context does not inherit process cancellation.
func NewContext(ctx context.Context, dir, password string, debug bool, debugStderr io.Writer) (*Sink, error) {
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("find diagnostic cache directory: %w", err)
		}
		dir = filepath.Join(cache, "imap-mcp")
	}
	return newSinkContext(ctx, dir, password, debug, debugStderr, maxFileSize)
}

func newSink(dir, password string, debug bool, debugStderr io.Writer, limit int64) (*Sink, error) {
	return newSinkContext(context.Background(), dir, password, debug, debugStderr, limit)
}

func newSinkContext(ctx context.Context, dir, password string, debug bool, debugStderr io.Writer, limit int64) (*Sink, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create diagnostic directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !owned(info) {
		return nil, fmt.Errorf("diagnostic directory is not an owned directory: %s", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s := &Sink{path: filepath.Join(dir, "imap-mcp.log"), root: root, done: make(chan struct{}), ctx: ctx, limit: limit, redactor: newRedactor(password)}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = errors.New("diagnostic directory changed during open")
	}
	if err == nil {
		err = root.Chmod(".", 0700)
	}
	if err == nil {
		s.lock, err = privateFile(root, "imap-mcp.log.lock", os.O_CREATE|os.O_RDWR)
	}
	if err == nil {
		err = s.prepare()
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("prepare diagnostic log: %w", err), s.Close())
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			return slog.String(slog.TimeKey, a.Value.Time().In(time.Local).Format(timestampFormat))
		}
		return a
	}}
	writer := &contextWriter{sink: s, gate: make(chan struct{}, 1)}
	handler := slog.NewJSONHandler(writer, opts)
	var stderr slog.Handler
	if debug && debugStderr != nil {
		stderr = slog.NewJSONHandler(stderrWriter{s, debugStderr}, opts)
	}
	s.logger = slog.New(contextHandler{teeHandler{handler, stderr}, writer})
	return s, nil
}

func (s *Sink) prepare() (result error) {
	if err := s.acquire(s.ctx); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, s.unlock()) }()
	if err := s.validateFiles(); err != nil {
		return err
	}
	for i := 0; i <= backups; i++ {
		if err := normalizeFile(s.root, filepath.Base(s.logPath(i)), s.limit); err != nil {
			return err
		}
	}
	return s.open()
}

func (s *Sink) Logger() *slog.Logger { return s.logger }
func (s *Sink) Path() string         { return s.path }

func (s *Sink) logPath(index int) string {
	if index == 0 {
		return s.path
	}
	return fmt.Sprintf("%s.%d", s.path, index)
}

// Existing oversized files retain only complete trailing records. Reading the
// tail before truncation keeps memory and disk bounded without a second file.
func normalizeFile(root *os.Root, path string, limit int64) (result error) {
	_, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := privateFile(root, path, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	info, err := f.Stat()
	if err != nil || info.Size() <= limit {
		return err
	}
	start := info.Size() - limit
	tail := make([]byte, limit)
	if _, err := f.ReadAt(tail, start); err != nil {
		return err
	}
	// The byte just before the tail tells us whether its first record is whole.
	var previous [1]byte
	if _, err := f.ReadAt(previous[:], start-1); err != nil {
		return err
	}
	if previous[0] != '\n' {
		if end := bytes.IndexByte(tail, '\n'); end >= 0 {
			tail = tail[end+1:]
		} else {
			tail = nil
		}
	}
	if end := bytes.LastIndexByte(tail, '\n'); end >= 0 {
		tail = tail[:end+1]
	} else {
		tail = nil
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(tail, 0)
	return err
}

func (s *Sink) open() error {
	f, err := privateFile(s.root, filepath.Base(s.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	s.file, s.size = f, info.Size()
	return nil
}

func (s *Sink) rotate() error {
	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil
	if err := s.root.Remove(filepath.Base(s.logPath(backups))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := backups - 1; i >= 0; i-- {
		if err := s.root.Rename(filepath.Base(s.logPath(i)), filepath.Base(s.logPath(i+1))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.open()
}

func (s *Sink) Write(p []byte) (int, error) {
	return s.write(context.Background(), p)
}

func (s *Sink) write(ctx context.Context, p []byte) (written int, result error) {
	for !s.mu.TryLock() {
		if err := s.wait(ctx); err != nil {
			return 0, err
		}
	}
	defer s.mu.Unlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	record, err := s.record(p)
	if err != nil {
		return 0, s.failWrite(err)
	}
	if err := s.acquire(ctx); err != nil {
		if errors.Is(err, os.ErrClosed) || ctx.Err() != nil || s.ctx.Err() != nil {
			return 0, err
		}
		return 0, s.failWrite(err)
	}
	defer func() {
		if err := s.unlock(); err != nil {
			result = errors.Join(result, s.failWrite(err))
		}
	}()
	if err := s.refresh(); err != nil {
		return 0, s.failWrite(err)
	}
	if s.size+int64(len(record)) > s.limit {
		if err := s.rotate(); err != nil {
			return 0, s.failWrite(err)
		}
	}
	n, err := s.file.Write(record)
	s.size += int64(n)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, s.failWrite(err)
	}
	return len(p), nil
}

func (s *Sink) failWrite(err error) error {
	s.writeErr = err
	s.err = errors.Join(s.err, err)
	return err
}

func (s *Sink) record(p []byte) ([]byte, error) {
	record := p
	if int64(len(record)) <= s.limit {
		record = s.redactor.redactJSON(record)
	}
	if int64(len(record)) <= s.limit {
		return record, nil
	}
	record, _ = json.Marshal(map[string]any{
		"time":         time.Now().In(time.Local).Format(timestampFormat),
		"level":        "WARN",
		"msg":          "diagnostic record omitted: exceeds log file limit",
		"record_bytes": len(p),
	})
	record = s.redactor.redactJSON(append(record, '\n'))
	if int64(len(record)) > s.limit {
		return nil, errors.New("diagnostic log limit cannot hold omission record")
	}
	return record, nil
}

// Close waits for active writes and returns any retained write or close error.
// Calls after the first Close return the same result.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		if s.file != nil {
			s.err = errors.Join(s.err, s.file.Close())
			s.file = nil
		}
		if s.lock != nil {
			s.err = errors.Join(s.err, s.lock.Close())
			s.lock = nil
		}
		if s.root != nil {
			s.err = errors.Join(s.err, s.root.Close())
			s.root = nil
		}
	}
	return s.err
}

// The stable lock is never rotated or removed: see docs/adr/0010-process-diagnostics-and-progress.md.
func (s *Sink) acquire(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.done:
			return os.ErrClosed
		default:
		}
		err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			info, statErr := s.lock.Stat()
			pathInfo, pathErr := s.root.Lstat("imap-mcp.log.lock")
			if statErr == nil && pathErr == nil && safeFile(info) && os.SameFile(info, pathInfo) {
				return nil
			}
			return errors.Join(errors.New("diagnostic lock path changed or is unsafe"), statErr, pathErr, s.unlock())
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if err := s.wait(ctx); err != nil {
			return err
		}
	}
}

func (s *Sink) wait(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-s.done:
		return os.ErrClosed
	case <-timer.C:
		return nil
	}
}

func (s *Sink) unlock() error { return syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN) }

func (s *Sink) validateFiles() error {
	for i := 0; i <= backups; i++ {
		info, err := s.root.Lstat(filepath.Base(s.logPath(i)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !safeFile(info) {
			return fmt.Errorf("diagnostic log is not an owned, single-link regular file: %s", s.logPath(i))
		}
	}
	return nil
}

// Another sink can rename the active inode while this sink is idle.
func (s *Sink) refresh() error {
	if err := s.validateFiles(); err != nil {
		return err
	}
	info, err := s.root.Lstat(filepath.Base(s.path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current, statErr := s.file.Stat()
	if statErr != nil {
		return statErr
	}
	if err != nil || !os.SameFile(info, current) {
		if err := s.file.Close(); err != nil {
			return err
		}
		s.file = nil
		return s.open()
	}
	if err := s.file.Chmod(0600); err != nil {
		return err
	}
	s.size = current.Size()
	return nil
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func safeFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && owned(info) && stat.Nlink == 1
}

func privateFile(root *os.Root, name string, flags int) (*os.File, error) {
	if info, err := root.Lstat(name); err == nil && !safeFile(info) {
		return nil, fmt.Errorf("unsafe diagnostic file: %s", name)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	pathInfo, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || !safeFile(info) || !os.SameFile(info, pathInfo) {
		return nil, errors.Join(errors.New("unsafe or changed diagnostic file"), statErr, pathErr, f.Close())
	}
	if err := f.Chmod(0600); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

type contextWriter struct {
	gate chan struct{}
	sink *Sink
	ctx  context.Context
}

func (w *contextWriter) Write(p []byte) (int, error) { return w.sink.write(w.ctx, p) }

type contextHandler struct {
	slog.Handler
	writer *contextWriter
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	// SDK request contexts may outlive root cancellation; see docs/adr/0010-process-diagnostics-and-progress.md.
	select {
	case h.writer.gate <- struct{}{}:
		defer func() { <-h.writer.gate }()
	case <-ctx.Done():
		return ctx.Err()
	case <-h.writer.sink.ctx.Done():
		return h.writer.sink.ctx.Err()
	case <-h.writer.sink.done:
		return os.ErrClosed
	}
	h.writer.ctx = ctx
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Handler = h.Handler.WithAttrs(attrs)
	return h
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	return h
}

type stderrWriter struct {
	sink   *Sink
	writer io.Writer
}

func (w stderrWriter) Write(p []byte) (int, error) {
	for !w.sink.mu.TryLock() {
		if err := w.sink.wait(w.sink.ctx); err != nil {
			return 0, err
		}
	}
	defer w.sink.mu.Unlock()
	if w.sink.closed {
		return 0, os.ErrClosed
	}
	record, err := w.sink.record(p)
	if err != nil {
		w.sink.err = errors.Join(w.sink.err, err)
		return 0, err
	}
	n, err := w.writer.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil && w.sink.err == nil {
		w.sink.err = err
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

type teeHandler struct {
	slog.Handler
	stderr slog.Handler
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if ctx.Err() != nil || errors.Is(err, os.ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if h.stderr != nil && r.Level == slog.LevelDebug && (r.Message == "stage" || r.Message == "duration") {
		err = errors.Join(err, h.stderr.Handle(ctx, r))
	}
	return err
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Handler = h.Handler.WithAttrs(attrs)
	if h.stderr != nil {
		h.stderr = h.stderr.WithAttrs(attrs)
	}
	return h
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	if h.stderr != nil {
		h.stderr = h.stderr.WithGroup(name)
	}
	return h
}
