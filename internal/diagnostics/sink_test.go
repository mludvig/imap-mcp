package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Fatal("log contains an incomplete trailing record")
	}
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal("log contains invalid JSON")
		}
		records = append(records, record)
	}
	return records
}

func assertBound(t *testing.T, dir string, limit int64) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > backups+2 {
		t.Fatalf("retained %d files", len(entries))
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name() == "imap-mcp.log.lock" {
			if info.Size() != 0 || info.Mode().Perm() != 0600 {
				t.Fatal("lock must be empty and owner-only")
			}
			continue
		}
		if info.Size() > limit {
			t.Fatalf("file exceeds limit: %d", info.Size())
		}
		total += info.Size()
		readRecords(t, filepath.Join(dir, entry.Name()))
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions = %o", info.Mode().Perm())
		}
	}
	if total > int64(backups+1)*limit {
		t.Fatalf("logs exceed total bound: %d", total)
	}
}

func TestIndependentSinksRefreshSizeAndActiveInode(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := New(dir, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	// Each record fits alone; two cannot share the production 10 MiB file.
	for index, sink := range []*Sink{first, second, first, second, first, second} {
		sink.Logger().Info(fmt.Sprintf("record-%d", index), "detail", strings.Repeat("x", 6*1024*1024))
		assertBound(t, dir, maxFileSize)
	}
	if err := errors.Join(first.Close(), second.Close()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Logger().Info("reopened")
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, maxFileSize)
	for i := 0; i <= backups; i++ {
		records := readRecords(t, first.logPath(i))
		if records[0]["msg"] != fmt.Sprintf("record-%d", 5-i) {
			t.Fatalf("backup %d has records in the wrong order", i)
		}
		if i == 0 && (len(records) != 2 || records[1]["msg"] != "reopened") {
			t.Fatal("reopen did not append to the active file")
		}
	}
}

// Runs only in the subprocess started by TestProcessesShareRotation.
func TestProcessLogWriter(t *testing.T) {
	dir := os.Getenv("IMAP_MCP_DIAGNOSTICS_TEST_DIR")
	if dir == "" {
		return
	}
	s, err := newSink(dir, "", false, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		s.Logger().Info("process record", "worker", os.Getenv("IMAP_MCP_DIAGNOSTICS_TEST_WORKER"), "index", index, "detail", strings.Repeat("x", 200))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessesShareRotation(t *testing.T) {
	dir := t.TempDir()
	// Concurrent startup also normalizes existing oversized files under the lock.
	for i := 0; i <= backups; i++ {
		name := filepath.Join(dir, "imap-mcp.log")
		if i > 0 {
			name += fmt.Sprintf(".%d", i)
		}
		if err := os.WriteFile(name, bytes.Repeat([]byte("{\"msg\":\"old\"}\n"), 100), 0644); err != nil {
			t.Fatal(err)
		}
	}
	type child struct {
		cmd    *exec.Cmd
		input  io.WriteCloser
		output *bufio.Reader
		stderr bytes.Buffer
	}
	var children []*child
	for _, worker := range []string{"first", "second"} {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestProcessLogWriter$", "-test.timeout=15s")}
		c.cmd.Env = append(os.Environ(), "IMAP_MCP_DIAGNOSTICS_TEST_DIR="+dir, "IMAP_MCP_DIAGNOSTICS_TEST_WORKER="+worker)
		c.cmd.Stderr = &c.stderr
		var err error
		c.input, err = c.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := c.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		c.output = bufio.NewReader(output)
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.input.Close(); c.cmd.Process.Kill() })
		children = append(children, c)
	}
	for _, c := range children {
		if ready, err := c.output.ReadString('\n'); err != nil || ready != "ready\n" {
			t.Fatalf("writer did not open: %q %v", ready, err)
		}
	}
	for _, c := range children {
		if _, err := io.WriteString(c.input, "start\n"); err != nil {
			t.Fatal(err)
		}
		c.input.Close()
	}
	for _, c := range children {
		remaining, err := io.ReadAll(c.output)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("writer failed: %v, %s, %s", err, remaining, c.stderr.String())
		}
	}
	assertBound(t, dir, 1024)
	latest := map[string]int{"first": -1, "second": -1}
	for i := backups; i >= 0; i-- {
		name := filepath.Join(dir, "imap-mcp.log")
		if i > 0 {
			name += fmt.Sprintf(".%d", i)
		}
		for _, record := range readRecords(t, name) {
			worker := record["worker"].(string)
			index := int(record["index"].(float64))
			if index <= latest[worker] {
				t.Fatal("process records are duplicated or reordered across backups")
			}
			latest[worker] = index
		}
	}
	if latest["first"] != 99 && latest["second"] != 99 {
		t.Fatal("newest completed process record was lost")
	}
	reopened, err := newSink(dir, "", false, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Logger().Info("reopened after processes")
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, 1024)
}

func TestLockWaitCancellationAndClose(t *testing.T) {
	s, err := newSink(t.TempDir(), "", false, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	lock, err := os.OpenFile(s.path+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Logger().With("bound", "attribute").WithGroup("group").InfoContext(ctx, "canceled lock wait")
	if time.Since(start) > time.Second || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("logging did not honor canceled context while waiting for the lock")
	}
	background := make(chan struct{})
	go func() { s.Logger().Info("background lock wait"); close(background) }()
	// A canceled log must also escape the handler queue behind a lock waiter.
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer queuedCancel()
	queued := make(chan struct{})
	go func() { s.Logger().InfoContext(queuedCtx, "queued canceled log"); close(queued) }()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("canceled logging deadlocked behind a queued handler")
	}
	written := make(chan error, 1)
	go func() { _, err := s.Write([]byte("{}\n")); written <- err }()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked behind a lock waiter")
	}
	if err := <-written; !errors.Is(err, os.ErrClosed) {
		t.Fatalf("interrupted write error = %v", err)
	}
	select {
	case <-background:
	case <-time.After(time.Second):
		t.Fatal("Close did not release the background logger")
	}
}

func TestRootCancellationInterruptsBackgroundLoggingAndQueues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := NewContext(ctx, t.TempDir(), "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	lock, err := os.OpenFile(s.Path()+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	logged := make(chan struct{}, 2)
	go func() { s.Logger().Info("background lock waiter"); logged <- struct{}{} }()
	// Wait for the first handler to own the write mutex and wait on flock.
	deadline := time.Now().Add(time.Second)
	for s.mu.TryLock() {
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("logger did not reach the write lock")
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		Stage(WithLogger(context.WithoutCancel(ctx), s.Logger().With("tool", "fixture").WithGroup("group")), "queued stage")
		logged <- struct{}{}
	}()
	written := make(chan error, 1)
	go func() { _, err := s.Write([]byte("{}\n")); written <- err }()
	cancel()
	for range 2 {
		select {
		case <-logged:
		case <-time.After(time.Second):
			t.Fatal("process cancellation did not release a background or queued logger")
		}
	}
	select {
	case err := <-written:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("direct write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("process cancellation did not release direct write")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("cancellation retained a write error: %v", err)
	}
}

func TestRootCancellationInterruptsLocalWriteQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := NewContext(ctx, t.TempDir(), "", true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.mu.Lock()
	finished := make(chan struct{})
	go func() { Stage(WithLogger(context.Background(), s.Logger()), "queued"); close(finished) }()
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("process cancellation did not release local write queue")
	}
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConstructorLockWaitHonorsRootCancellation(t *testing.T) {
	dir := t.TempDir()
	lock, err := os.OpenFile(filepath.Join(dir, "imap-mcp.log.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan error, 1)
	go func() {
		s, err := NewContext(ctx, dir, "", false, nil)
		if s != nil {
			_ = s.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("constructor did not wait on the held lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-opened:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("constructor cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("constructor stayed blocked after root cancellation")
	}
}

func TestCanceledLoggingEscapesLocalWriteQueue(t *testing.T) {
	var stderr bytes.Buffer
	s, err := newSink(t.TempDir(), "", true, &stderr, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Stand in for an active direct Sink.Write, outside the slog handler gate.
	s.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	finished := make(chan struct{})
	go func() { Stage(WithLogger(ctx, s.Logger()), "canceled queued stage"); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("canceled log deadlocked behind the local write mutex")
	}
	s.mu.Unlock()
	if stderr.Len() != 0 {
		t.Fatal("canceled queued stage reached stderr")
	}
}

func TestRotationRetainsWholeNewestRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := newSink(dir, "", false, nil, 180)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		s.Logger().Info(fmt.Sprintf("record-%02d", i), "detail", strings.Repeat("x", 40))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, s.limit)
	for i := 0; i <= backups; i++ {
		records := readRecords(t, s.logPath(i))
		if len(records) != 1 || records[0]["msg"] != fmt.Sprintf("record-%02d", 11-i) {
			t.Fatalf("backup %d does not retain the expected whole record", i)
		}
	}
}

func TestOversizedRecordIsOmittedAndLoggingContinues(t *testing.T) {
	dir := t.TempDir()
	s, err := newSink(dir, "", false, nil, 180)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info(strings.Repeat("oversized", 1000))
	s.Logger().Info("next")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, s.limit)
	records := append(readRecords(t, s.logPath(1)), readRecords(t, s.Path())...)
	if len(records) != 2 || records[0]["level"] != "WARN" || records[1]["msg"] != "next" {
		t.Fatal("oversized record did not produce one omission record followed by the next record")
	}
	if records[0]["record_bytes"].(float64) <= float64(s.limit) {
		t.Fatal("omission record does not describe the oversized input")
	}
}

func TestRedactionExpansionAndDebugStderrStayBounded(t *testing.T) {
	var stderr bytes.Buffer
	s, err := newSink(t.TempDir(), "fixture", true, &stderr, 256)
	if err != nil {
		t.Fatal(err)
	}
	// The original record fits; redaction makes it exceed the file limit.
	s.Logger().Info(strings.Repeat("fixture", 25))
	Stage(WithLogger(context.Background(), s.Logger()), strings.Repeat("fixture", 1000))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, filepath.Dir(s.Path()), s.limit)
	if stderr.Len() > int(s.limit) {
		t.Fatal("oversized debug stage exceeded the stderr record bound")
	}
	for i := 0; i <= 1; i++ {
		records := readRecords(t, s.logPath(i))
		if len(records) != 1 || records[0]["level"] != "WARN" {
			t.Fatal("oversized redacted record was not replaced")
		}
	}
	if bytes.Contains(stderr.Bytes(), []byte("fixture")) {
		t.Fatal("oversized debug stage leaked its password")
	}
}

func TestExistingFilesAreBoundedAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	record := []byte("{\"msg\":\"retained\"}\n")
	for i := 0; i <= backups; i++ {
		path := filepath.Join(dir, "imap-mcp.log")
		if i > 0 {
			path += fmt.Sprintf(".%d", i)
		}
		if err := os.WriteFile(path, bytes.Repeat(record, 100), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := newSink(dir, "", false, nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, s.limit)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("directory permissions = %o", info.Mode().Perm())
	}
	s.Logger().Info("current")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertBound(t, dir, s.limit)
}

func TestExistingOversizedPartialRecordIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "imap-mcp.log"), []byte(strings.Repeat("x", 1024)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newSink(dir, "", false, nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info("current")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	records := readRecords(t, s.Path())
	if len(records) != 1 || records[0]["msg"] != "current" {
		t.Fatal("partial oversized record was retained")
	}
}

func TestPasswordRedactionPreservesOtherData(t *testing.T) {
	// Never include the fixture or complete log in failure output.
	password := "fixture-/\"\\\n\t<>&\u2028-password"
	encoded, _ := json.Marshal(password)
	var unicodeEscaped strings.Builder
	unicodeEscaped.WriteByte('"')
	for _, character := range password {
		fmt.Fprintf(&unicodeEscaped, "\\u%04x", character)
	}
	unicodeEscaped.WriteByte('"')
	var noHTML bytes.Buffer
	encoder := json.NewEncoder(&noHTML)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(password)
	var stderr bytes.Buffer
	s, err := New(t.TempDir(), password, true, &stderr)
	if err != nil {
		t.Fatal("could not create diagnostic sink")
	}
	logger := s.Logger().With("bound", password).WithGroup("request")
	logger.Error("failed "+password, "error", errors.New("denied "+password),
		"nested", map[string]any{"credential": password, "username": "user@example.test"},
		"json", string(encoded), "json_no_html", noHTML.String(),
		"json_unicode", unicodeEscaped.String(), "json_slash", strings.ReplaceAll(string(encoded), "/", `\/`),
		"host", "imap.example.test", "mailbox", "INBOX", "query", "subject:invoice")
	ctx := WithLogger(context.Background(), logger)
	Stage(ctx, "auth "+password+" "+string(encoded))
	Measure(ctx, "stage "+password)()
	if err := s.Close(); err != nil {
		t.Fatal("could not close diagnostic sink")
	}
	file, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{file, stderr.Bytes()} {
		for _, variant := range s.redactor.passwords {
			if bytes.Contains(data, []byte(variant)) {
				t.Fatal("password representation leaked")
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		for {
			var decoded any
			if err := decoder.Decode(&decoded); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal("redacted record contains invalid JSON")
			}
			assertNoPassword(t, decoded, s.redactor)
		}
		if !bytes.Contains(data, []byte("[REDACTED]")) {
			t.Fatal("redaction marker is missing")
		}
	}
	for _, want := range []string{"user@example.test", "imap.example.test", "INBOX", "subject:invoice"} {
		if !bytes.Contains(file, []byte(want)) {
			t.Fatalf("non-password field %q was not preserved", want)
		}
	}
}

func assertNoPassword(t *testing.T, value any, r redactor) {
	t.Helper()
	switch value := value.(type) {
	case string:
		for _, variant := range r.passwords {
			if strings.Contains(value, variant) {
				t.Fatal("decoded log contains password representation")
			}
		}
		var embedded any
		if json.Unmarshal([]byte(value), &embedded) == nil {
			assertNoPassword(t, embedded, r)
		}
	case map[string]any:
		for key, item := range value {
			assertNoPassword(t, key, r)
			assertNoPassword(t, item, r)
		}
	case []any:
		for _, item := range value {
			assertNoPassword(t, item, r)
		}
	}
}

func TestPasswordInsideRedactionMarkerIsRemoved(t *testing.T) {
	password := "REDACTED"
	s, err := New(t.TempDir(), password, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info("failed " + password)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(password)) {
		t.Fatal("redaction marker contains the password")
	}
}

func TestLocalTimestampAndDebugFiltering(t *testing.T) {
	prior := time.Local
	time.Local = time.FixedZone("test-local", 13*60*60)
	t.Cleanup(func() { time.Local = prior })
	for _, debug := range []bool{false, true} {
		var stderr bytes.Buffer
		s, err := New(t.TempDir(), "", debug, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		var progress []string
		ctx := WithProgress(WithLogger(context.Background(), s.Logger()), func(message string) {
			progress = append(progress, message)
		})
		s.Logger().Info("lifecycle")
		s.Logger().Info("user timestamp attribute", "time", "arbitrary text")
		s.Logger().Debug("other detail")
		Stage(ctx, "connecting")
		Measure(ctx, "connection")()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		records := readRecords(t, s.Path())
		wantCount := 2
		if debug {
			wantCount = 5
		}
		if len(records) != wantCount || len(progress) != 1 || progress[0] != "connecting" {
			t.Fatal("debug level or progress callback did not match")
		}
		for _, record := range records {
			if record["msg"] == "user timestamp attribute" {
				if record["time"] != "arbitrary text" {
					t.Fatal("user time attribute was changed")
				}
				continue
			}
			timestamp := record["time"].(string)
			parsed, err := time.Parse(time.RFC3339, timestamp)
			if err != nil || !strings.HasSuffix(timestamp, "+13:00") {
				t.Fatal("timestamp does not have local RFC3339 offset")
			}
			if time.Since(parsed) > 2*time.Second {
				t.Fatal("timestamp does not represent the current instant")
			}
		}
		if debug {
			var messages []string
			decoder := json.NewDecoder(&stderr)
			for {
				var record map[string]any
				if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				messages = append(messages, record["msg"].(string))
			}
			if strings.Join(messages, ",") != "stage,duration" {
				t.Fatal("stderr received non-stage/debug timing messages")
			}
		} else if stderr.Len() != 0 {
			t.Fatal("normal mode wrote debug stderr")
		}
	}
}

func TestUTCOffsetIsExplicitIncludingOmissionRecords(t *testing.T) {
	prior := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prior })
	s, err := newSink(t.TempDir(), "", false, nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info(strings.Repeat("x", 1024))
	s.Logger().Info("normal record")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	records := readRecords(t, s.Path())
	if len(records) != 2 || records[0]["level"] != "WARN" {
		t.Fatal("expected both omission and normal records")
	}
	for _, record := range records {
		timestamp := record["time"].(string)
		if _, err := time.Parse(time.RFC3339, timestamp); err != nil || !strings.HasSuffix(timestamp, "+00:00") {
			t.Fatal("UTC timestamp does not contain an explicit numeric offset")
		}
	}
}

func TestConcurrentRotationAndClose(t *testing.T) {
	s, err := newSink(t.TempDir(), "", true, nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < 100; i++ {
				s.Logger().Info("concurrent record", "index", i)
			}
		}()
	}
	writers.Wait()
	var closers sync.WaitGroup
	for i := 0; i < 8; i++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	closers.Wait()
	if _, err := s.Write([]byte("{}\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatal("write after close did not return ErrClosed")
	}
	assertBound(t, filepath.Dir(s.Path()), s.limit)
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCloseReportsRetainedWriteErrors(t *testing.T) {
	want := errors.New("diagnostic writer failure")
	s, err := New(t.TempDir(), "", true, failingWriter{want})
	if err != nil {
		t.Fatal(err)
	}
	Stage(WithLogger(context.Background(), s.Logger()), "connecting")
	s.Logger().Info("file still records after stderr failure")
	for i := 0; i < 2; i++ {
		if !errors.Is(s.Close(), want) {
			t.Fatal("Close did not retain stderr error")
		}
	}
	records := readRecords(t, s.Path())
	if len(records) != 2 || records[1]["msg"] != "file still records after stderr failure" {
		t.Fatal("stderr failure stopped file logging")
	}
	s, err = New(t.TempDir(), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	s.Logger().Info("write failure")
	if !errors.Is(s.Close(), os.ErrClosed) {
		t.Fatal("Close did not retain file write error")
	}
}

func TestDefaultPathRespectsUserCacheDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux XDG cache directory")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	s, err := New("", "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Path() != filepath.Join(dir, "imap-mcp", "imap-mcp.log") {
		t.Fatal("default path did not use XDG cache")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if maxFileSize != 10*1024*1024 || backups != 4 {
		t.Fatal("production retention budget changed")
	}
}

func TestSymlinkLogIsRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "imap-mcp.log")); err != nil {
		t.Skip(err)
	}
	if _, err := New(dir, "", false, nil); err == nil {
		t.Fatal("symlink log was accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("symlink target was changed")
	}
}

func TestUnsafePathsDoNotChangeLinkedTargets(t *testing.T) {
	for _, name := range []string{"imap-mcp.log", "imap-mcp.log.1", "imap-mcp.log.4", "imap-mcp.log.lock"} {
		for _, hardlink := range []bool{false, true} {
			for _, afterOpen := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/hardlink=%t/after-open=%t", name, hardlink, afterOpen), func(t *testing.T) {
					dir := t.TempDir()
					target := filepath.Join(t.TempDir(), "unrelated")
					want := bytes.Repeat([]byte("unrelated data\n"), 100)
					if err := os.WriteFile(target, want, 0644); err != nil {
						t.Fatal(err)
					}
					var sink *Sink
					if afterOpen {
						var err error
						sink, err = newSink(dir, "", false, nil, 256)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { sink.Close() })
					}
					path := filepath.Join(dir, name)
					if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					link := os.Symlink
					if hardlink {
						link = os.Link
					}
					if err := link(target, path); err != nil {
						t.Fatal(err)
					}
					if afterOpen {
						if _, err := sink.Write([]byte("{}\n")); err == nil {
							t.Fatal("unsafe replaced path accepted")
						}
						if err := sink.Close(); err == nil {
							t.Fatal("unsafe-path write error was not retained")
						}
					} else if opened, err := newSink(dir, "", false, nil, 256); err == nil {
						opened.Close()
						t.Fatal("unsafe existing path accepted")
					}
					got, err := os.ReadFile(target)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatal("unrelated linked file was changed")
					}
					info, err := os.Stat(target)
					if err != nil || info.Mode().Perm() != 0644 {
						t.Fatal("unrelated linked file permissions were changed")
					}
				})
			}
		}
	}
}

func TestSymlinkDirectoryIsRejectedWithoutChangingTarget(t *testing.T) {
	target := t.TempDir()
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if opened, err := New(path, "", false, nil); err == nil {
		opened.Close()
		t.Fatal("symlink directory accepted")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("symlink directory target permissions were changed")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink directory target was changed")
	}
}

func TestContextWithoutLoggerIsSafe(t *testing.T) {
	ctx := context.Background()
	Stage(ctx, "offline")
	Measure(ctx, "offline")()
	Logger(WithLogger(ctx, (*slog.Logger)(nil))).Info("discarded")
}
