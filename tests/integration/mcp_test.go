//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var readToolNames = []string{
	"server_info", "list_mailboxes", "mailbox_status", "list_messages",
	"search_messages", "read_message", "get_attachment", "thread_messages",
}

var writeToolNames = []string{
	"add_flags", "remove_flags", "copy_messages", "move_messages",
	"trash_messages", "append_message",
}

// These tests cross the real stdio/process boundary; mailbox assertions use the
// independent EXAMINE/PEEK observer described in testbed/README.md.
func TestMCPDovecot(t *testing.T) {
	binary := buildMCPBinary(t)
	for _, rev2 := range []bool{false, true} {
		name := "rev1"
		if rev2 {
			name = "rev2"
		}
		t.Run(name, func(t *testing.T) {
			f := startDovecot(t, rev2)
			for _, mode := range []string{"tls", "starttls"} {
				t.Run(mode+"/read-tools", func(t *testing.T) {
					configFile, cachePath := mcpFixtureConfig(t, f, mode, true, "Trash")
					process := startMCP(t, binary, configFile)
					assertMCPRegistry(t, process, true)
					exerciseMCPReads(t, f, process, true)
					assertCacheOwned(t, cachePath)
					process.close(t)
					assertCacheReleased(t, cachePath)
					// Also exercise signal shutdown of an initialized real session.
					if mode == "tls" {
						process = startMCP(t, binary, configFile)
						if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
						process.close(t)
						assertCacheReleased(t, cachePath)
					}
				})
			}
			t.Run("startup-mode-and-writes", func(t *testing.T) {
				mode := "tls"
				if rev2 {
					mode = "starttls"
				}
				testMCPStartupAndWrites(t, f, binary, mode)
			})
			// Last: stopping this test's container also tests the no-offline rule.
			t.Run("persistent-cache-live-refresh", func(t *testing.T) {
				testMCPCacheRefresh(t, f, binary)
			})
		})
	}
}

func buildMCPBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	goBinary := filepath.Join(root, ".tools", "go", "bin", "go")
	if _, err := os.Stat(goBinary); err != nil {
		goBinary, err = exec.LookPath("go")
		if err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(t.TempDir(), "imap-mcp")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBinary, "build", "-mod=readonly", "-o", binary, "./cmd/imap-mcp")
	cmd.Dir = root
	// Keep build artifacts inside the repository, as required by Makefile.
	cmd.Env = append(os.Environ(),
		"GOTOOLCHAIN=local",
		"GOPATH="+filepath.Join(root, ".tools", "cache", "gopath"),
		"GOMODCACHE="+filepath.Join(root, ".tools", "cache", "mod"),
		"GOCACHE="+filepath.Join(root, ".tools", "cache", "build"),
		"TMPDIR="+filepath.Join(root, ".tools", "tmp"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build real MCP CLI: %v\n%s", err, output)
	}
	return binary
}

func mcpFixtureConfig(t *testing.T, f *dovecotFixture, mode string, readonly bool, trash string) (string, string) {
	t.Helper()
	path := f.ConfigFile(t, mode, readonly)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(filepath.Dir(path), "headers.sqlite3")
	body := strings.Replace(string(data), `trash_mailbox = "Trash"`, fmt.Sprintf("trash_mailbox = %q", trash), 1)
	body += fmt.Sprintf("\n[cache]\npath = %q\n", cachePath)
	writeFixtureFile(t, path, []byte(body))
	return path, cachePath
}

func setMCPReadonly(t *testing.T, path string, old, next bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, after := fmt.Sprintf("readonly = %t", old), fmt.Sprintf("readonly = %t", next)
	if strings.Count(string(data), before) != 1 {
		t.Fatalf("expected exactly one %q in config", before)
	}
	writeFixtureFile(t, path, []byte(strings.Replace(string(data), before, after, 1)))
}

type mcpProcess struct {
	session *mcp.ClientSession
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	closed  bool
	secret  string
}

func startMCP(t *testing.T, binary, configFile string) *mcpProcess {
	t.Helper()
	p := &mcpProcess{cmd: exec.Command(binary, "--config", configFile)}
	p.cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+t.TempDir())
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	p.secret = cfg.IMAP().Password()
	p.cmd.Stderr = &p.stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "imap-mcp-integration", Version: "test"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: p.cmd, TerminateDuration: 3 * time.Second}, nil)
	if err != nil {
		t.Fatalf("connect real MCP CLI: %v\n%s", err, p.stderr.String())
	}
	p.session = session
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *mcpProcess) close(t *testing.T) {
	t.Helper()
	if p.closed {
		return
	}
	p.closed = true
	if err := p.session.Close(); err != nil {
		t.Errorf("MCP shutdown: %v\n%s", err, p.stderr.String())
	}
	if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Success() {
		t.Errorf("MCP CLI did not exit successfully: %v\n%s", p.cmd.ProcessState, p.stderr.String())
	}
	if strings.Contains(p.stderr.String(), p.secret) {
		t.Error("MCP CLI exposed the configured password on stderr")
	}
}

func (p *mcpProcess) call(t *testing.T, name string, arguments any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s transport/protocol error: %v\n%s", name, err, p.stderr.String())
	}
	return result
}

func mcpData[T any](t *testing.T, p *mcpProcess, name string, arguments any) T {
	t.Helper()
	result := p.call(t, name, arguments)
	if result.IsError {
		data, _ := json.Marshal(result.StructuredContent)
		t.Fatalf("%s failed: %s", name, data)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *mail.Error     `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Data) == 0 || envelope.Error != nil {
		t.Fatalf("%s expected structured data envelope, got %s (%v)", name, data, err)
	}
	var output T
	if err := json.Unmarshal(envelope.Data, &output); err != nil {
		t.Fatalf("%s decode structured data: %v: %s", name, err, envelope.Data)
	}
	return output
}

func assertMCPError(t *testing.T, p *mcpProcess, name string, arguments any, code string) {
	t.Helper()
	result := p.call(t, name, arguments)
	if !result.IsError {
		t.Fatalf("%s unexpectedly succeeded: %#v", name, result.StructuredContent)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *mail.Error     `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Error == nil ||
		envelope.Error.Code == "" || envelope.Error.Message == "" || envelope.Error.OutcomeUnknown ||
		(code != "" && envelope.Error.Code != code) || len(envelope.Data) != 0 {
		t.Fatalf("%s expected certain %s error, got %s (%v)", name, code, data, err)
	}
}

func assertMCPRegistry(t *testing.T, p *mcpProcess, readonly bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := p.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), readToolNames...)
	if !readonly {
		want = append(want, writeToolNames...)
	}
	got := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		got = append(got, tool.Name)
		if tool.InputSchema == nil {
			t.Errorf("%s missing input schema", tool.Name)
		}
		assertMCPInputSchema(t, tool)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) || result.NextCursor != "" {
		t.Fatalf("registered tools = %v, want %v; next cursor %q", got, want, result.NextCursor)
	}
}

func assertMCPInputSchema(t *testing.T, tool *mcp.Tool) {
	t.Helper()
	data, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	required := map[string][]string{
		"read_message":   {"ref"},
		"get_attachment": {"ref", "part_id"},
		"add_flags":      {"refs", "flags"},
		"remove_flags":   {"refs", "flags"},
		"copy_messages":  {"refs", "destination"},
		"move_messages":  {"refs", "destination"},
		"trash_messages": {"refs"},
		"append_message": {"message_base64"},
	}
	want := append([]string(nil), required[tool.Name]...)
	sort.Strings(want)
	sort.Strings(schema.Required)
	if strings.Join(schema.Required, ",") != strings.Join(want, ",") {
		t.Errorf("%s required input fields = %v, want %v", tool.Name, schema.Required, want)
	}
	for _, field := range want {
		if _, exists := schema.Properties[field]; !exists {
			t.Errorf("%s lacks required property %q", tool.Name, field)
		}
	}
}

func assertCacheOwned(t *testing.T, path string) {
	t.Helper()
	c, err := cache.Open(path)
	if err == nil {
		c.Close()
		t.Fatal("running MCP process did not exclusively own its cache")
	}
	if !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("cache lock failure: %v", err)
	}
}

func assertCacheReleased(t *testing.T, path string) {
	t.Helper()
	c, err := cache.Open(path)
	if err != nil {
		t.Fatalf("cache lock retained after process exit: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func exerciseMCPReads(t *testing.T, f *dovecotFixture, p *mcpProcess, readonly bool) {
	t.Helper()
	before := fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"})
	info := mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
	if info.Readonly != readonly {
		t.Fatalf("server readonly = %t, want %t", info.Readonly, readonly)
	}
	for _, capability := range []string{"UIDPLUS", "CONDSTORE", "MOVE"} {
		if !containsCapability(info.ImplementedFeatures, capability) {
			t.Errorf("missing implemented %s: %v", capability, info.ImplementedFeatures)
		}
	}
	if containsCapability(info.Capabilities, "IMAP4rev2") != f.Rev2 {
		t.Errorf("server reported wrong protocol revision: %v", info.Capabilities)
	}
	if f.Rev2 && !containsCapability(info.EnabledCapabilities, "IMAP4rev2") {
		t.Errorf("IMAP4rev2 was advertised but not enabled: %v", info.EnabledCapabilities)
	}
	if !f.Rev2 && containsCapability(info.Capabilities, "UTF8=ACCEPT") &&
		!containsCapability(info.EnabledCapabilities, "UTF8=ACCEPT") {
		t.Errorf("UTF8=ACCEPT was advertised but not enabled: %v", info.EnabledCapabilities)
	}
	mailboxes := mcpData[[]mail.MailboxInfo](t, p, "list_mailboxes", map[string]any{})
	names := map[string]bool{}
	for _, box := range mailboxes {
		names[box.Name] = true
	}
	for name := range before {
		if !names[name] {
			t.Errorf("missing mailbox %q: %v", name, names)
		}
	}
	status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{})
	if status.Mailbox != "INBOX" || int(status.Messages) != len(before["INBOX"]) || status.UIDValidity == 0 {
		t.Fatalf("unexpected INBOX status: %+v", status)
	}
	unseen := 0
	for _, message := range before["INBOX"] {
		if !containsFlag(message.Flags, imap.FlagSeen) {
			unseen++
		}
	}
	if int(status.Unseen) != unseen {
		t.Errorf("status unseen = %d, want %d", status.Unseen, unseen)
	}
	page := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{})
	if len(page.Messages) != 50 || page.Total != len(before["INBOX"]) || page.NextBeforeUID == 0 {
		t.Fatalf("default list pagination: %+v", page)
	}
	next := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"before_uid": page.NextBeforeUID})
	all := append(append([]mail.MessageSummary(nil), page.Messages...), next.Messages...)
	if len(all) != len(before["INBOX"]) || next.NextBeforeUID != 0 {
		t.Fatalf("second list page did not complete membership: %d messages, cursor %d", len(all), next.NextBeforeUID)
	}
	refs := map[uint32]mail.MessageRef{}
	var last uint32
	for i, summary := range all {
		if _, exists := refs[summary.Ref.UID]; exists || (i > 0 && summary.Ref.UID >= last) {
			t.Fatalf("pagination duplicated or misordered UID %d", summary.Ref.UID)
		}
		if summary.Ref.Mailbox != "INBOX" || summary.Ref.UIDValidity != status.UIDValidity {
			t.Fatalf("list reference lost mailbox epoch: %+v", summary.Ref)
		}
		observed, exists := before["INBOX"][imap.UID(summary.Ref.UID)]
		if !exists || summary.Size != int64(len(observed.Raw)) || !summary.InternalDate.Equal(observed.InternalDate) {
			t.Fatalf("list summary does not match independent observer: %+v", summary)
		}
		assertStringFlags(t, summary.Flags, observed.Flags)
		refs[summary.Ref.UID] = summary.Ref
		last = summary.Ref.UID
	}
	// Only these fields are supplied: empty optional fields must not be required.
	minimal := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{
		"subject": "Flagged fixture", "from": "alice@example.test", "since": "2025-01-15",
	})
	assertPageUIDs(t, minimal, f.UIDs["flagged"])
	flagged := minimal.Messages[0]
	if len(flagged.From) != 1 || !strings.Contains(flagged.From[0], "Alice É") ||
		len(flagged.Cc) != 1 || len(flagged.Bcc) != 1 || flagged.Date == nil {
		t.Fatalf("decoded header summary lost addresses/date: %+v", flagged)
	}
	date := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{
		"subject": "Date boundary", "since": "2025-01-15", "before": "2025-01-16",
	})
	assertPageUIDs(t, date, f.UIDs["boundary"])
	sortArgs := map[string]any{"subject": "Date boundary", "sort": "to_asc"}
	if containsCapability(info.Capabilities, "SORT") {
		sorted := mcpData[mail.MessagePage](t, p, "search_messages", sortArgs)
		assertPageUIDs(t, sorted, f.UIDs["before"], f.UIDs["boundary"], f.UIDs["after"])
	} else {
		assertMCPError(t, p, "search_messages", sortArgs, "unsupported")
	}
	header := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{
		"headers": map[string]string{"X-Fixture-ID": "unseen"},
	})
	assertPageUIDs(t, header, f.UIDs["unseen"])
	unicode := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{
		"mailbox": "旅行", "subject": "résumé 旅行",
	})
	assertPageUIDs(t, unicode, f.UIDs["unicode"])
	if unicode.Messages[0].Ref.Mailbox != "旅行" || unicode.Messages[0].Subject != "Résumé 旅行" {
		t.Fatalf("Unicode mailbox/header decode: %+v", unicode)
	}
	for _, live := range []string{"body", "text"} {
		found := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{live: "Fixture body for unseen."})
		assertPageUIDs(t, found, f.UIDs["unseen"])
	}
	for _, key := range []string{"fIxTuRe-KeYwOrD", "FIXTURE-KEYWORD"} {
		cached := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{"flags": []string{key}})
		live := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{"flags": []string{key}, "body": "Fixture"})
		assertPageUIDs(t, cached, f.UIDs["flagged"])
		assertPageUIDs(t, live, f.UIDs["flagged"])
	}
	plain := mcpData[mail.Message](t, p, "read_message", map[string]any{"ref": refs[uint32(f.UIDs["unseen"])]})
	if plain.Text != "Fixture body for unseen.\r\n" || plain.HTML != "" || plain.Truncated {
		t.Fatalf("plain message result: %+v", plain)
	}
	short := mcpData[mail.Message](t, p, "read_message", map[string]any{
		"ref": refs[uint32(f.UIDs["unseen"])], "max_text_chars": 5,
	})
	if short.Text != "Fixtu" || !short.Truncated {
		t.Fatalf("explicit text truncation: %+v", short)
	}
	multipart := mcpData[mail.Message](t, p, "read_message", map[string]any{
		"ref": refs[uint32(f.UIDs["mime"])], "include_html": true,
	})
	if strings.TrimSpace(multipart.Text) != "Plain café body." ||
		multipart.HTML != "<p>HTML café body.</p>" || len(multipart.Attachments) != 2 {
		t.Fatalf("multipart message result: %+v", multipart)
	}
	expectedAttachments := map[string]string{"résumé.txt": "attachment café\n", "notes é.txt": "Nested attachment café."}
	for _, attachment := range multipart.Attachments {
		data := mcpData[mail.Attachment](t, p, "get_attachment", map[string]any{
			"ref": refs[uint32(f.UIDs["mime"])], "part_id": attachment.PartID,
		})
		decoded, err := base64.StdEncoding.DecodeString(data.DataBase64)
		want, exists := expectedAttachments[attachment.Filename]
		if err != nil || !exists || string(decoded) != want || data.Info.Size != int64(len(decoded)) {
			t.Fatalf("attachment exact bytes for %q: %q (%v), metadata %+v", attachment.Filename, decoded, err, data.Info)
		}
	}
	threadArgs := map[string]any{"algorithm": "REFERENCES", "query": map[string]any{"subject": "Unread fixture"}}
	if containsCapability(info.Capabilities, "THREAD=REFERENCES") {
		thread := mcpData[mail.ThreadResult](t, p, "thread_messages", threadArgs)
		if len(thread.Threads) != 1 || thread.Threads[0].Ref != refs[uint32(f.UIDs["unseen"])] ||
			len(thread.Threads[0].Children) != 1 || thread.Threads[0].Children[0].Ref != refs[uint32(f.UIDs["reply"])] {
			t.Fatalf("REFERENCES thread lost fixture parent/reply or epochs: %+v", thread)
		}
	} else {
		assertMCPError(t, p, "thread_messages", threadArgs, "unsupported")
	}
	stale := refs[uint32(f.UIDs["unseen"])]
	stale.UIDValidity++
	assertMCPError(t, p, "read_message", map[string]any{"ref": stale}, "stale_reference")
	stale.UIDValidity = 0
	assertMCPError(t, p, "read_message", map[string]any{"ref": stale}, "invalid_argument")
	assertAccountSnapshot(t, before, fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"}))
}

func containsCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(capability, want) {
			return true
		}
	}
	return false
}

func fixtureSnapshots(t *testing.T, f *dovecotFixture, mailboxes []string) map[string]map[imap.UID]observedMessage {
	t.Helper()
	result := make(map[string]map[imap.UID]observedMessage, len(mailboxes))
	for _, name := range mailboxes {
		result[name] = f.Snapshot(t, name)
	}
	return result
}

func assertStringFlags(t *testing.T, got []string, want []imap.Flag) {
	t.Helper()
	canonical := func(values []string) []string {
		result := make([]string, len(values))
		for i, value := range values {
			result[i] = strings.ToLower(value)
		}
		sort.Strings(result)
		return result
	}
	if !reflect.DeepEqual(canonical(got), persistentFlags(want)) {
		t.Fatalf("summary flags = %v, observer flags = %v", got, want)
	}
}

func assertPageUIDs(t *testing.T, page mail.MessagePage, want ...imap.UID) {
	t.Helper()
	got := make([]uint32, len(page.Messages))
	expected := make([]uint32, len(want))
	for i, message := range page.Messages {
		got[i] = message.Ref.UID
		if message.Ref.UID == 0 || message.Ref.UIDValidity == 0 || message.Ref.Mailbox == "" ||
			message.Ref.UIDValidity != page.UIDValidity {
			t.Fatalf("search reference lost its epoch: %+v", message.Ref)
		}
	}
	for i, uid := range want {
		expected[i] = uint32(uid)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(expected, func(i, j int) bool { return expected[i] < expected[j] })
	if !reflect.DeepEqual(got, expected) || page.Total != len(want) {
		t.Fatalf("search UIDs = %v, total %d; want %v", got, page.Total, expected)
	}
}

func testMCPStartupAndWrites(t *testing.T, f *dovecotFixture, binary, mode string) {
	t.Helper()
	configFile, cachePath := mcpFixtureConfig(t, f, mode, true, "MCP-Trash")
	p := startMCP(t, binary, configFile)
	page := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"limit": 200})
	refs := map[uint32]mail.MessageRef{}
	for _, message := range page.Messages {
		refs[message.Ref.UID] = message.Ref
	}
	before := fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"})
	setMCPReadonly(t, configFile, true, false)
	assertMCPRegistry(t, p, true)
	info := mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
	if !info.Readonly {
		t.Fatal("editing config changed running readonly mode")
	}
	// Unknown tools may be returned as a protocol error by the SDK.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: "add_flags", Arguments: map[string]any{
		"refs": []mail.MessageRef{refs[uint32(f.UIDs["unseen"])]}, "flags": []string{`\Seen`},
	}})
	cancel()
	if err == nil && !result.IsError {
		t.Fatal("readonly process accepted an unregistered write tool")
	}
	assertAccountSnapshot(t, before, fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"}))
	p.close(t)
	assertCacheReleased(t, cachePath)
	p = startMCP(t, binary, configFile)
	assertMCPRegistry(t, p, false)
	exerciseMCPReads(t, f, p, false)
	setMCPReadonly(t, configFile, false, true)
	assertMCPRegistry(t, p, false)
	info = mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
	if info.Readonly {
		t.Fatal("editing config changed running mutable mode")
	}
	runMCPWrites(t, f, p, refs)
	p.close(t)
	assertCacheReleased(t, cachePath)
	p = startMCP(t, binary, configFile)
	assertMCPRegistry(t, p, true)
	info = mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
	if !info.Readonly {
		t.Fatal("readonly config was not applied after restart")
	}
	p.close(t)
}

func runMCPWrites(t *testing.T, f *dovecotFixture, p *mcpProcess, refs map[uint32]mail.MessageRef) {
	t.Helper()
	boxes := []string{"INBOX", "Archive", "Trash", "旅行"}
	// Give the remove-flags target unrelated flags that must survive removal.
	c := f.Dial(t, "tls")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.UIDSetNum(f.UIDs["page-00"]), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, "preserve-keyword"},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, operation := range []struct {
		name string
		id   string
		flag imap.Flag
		add  bool
	}{
		{"add_flags", "flagged", imap.FlagAnswered, true},
		{"remove_flags", "page-00", imap.FlagAnswered, false},
	} {
		before := fixtureSnapshots(t, f, boxes)
		ref := refs[uint32(f.UIDs[operation.id])]
		result := mcpData[mail.MutationResult](t, p, operation.name, map[string]any{
			"refs": []mail.MessageRef{ref}, "flags": []string{string(operation.flag)},
		})
		if !reflect.DeepEqual(result.Affected, []mail.MessageRef{ref}) || result.OutcomeUnknown {
			t.Fatalf("%s affected identity/outcome: %+v", operation.name, result)
		}
		after := fixtureSnapshots(t, f, boxes)
		expected := before["INBOX"][imap.UID(ref.UID)]
		if operation.add {
			expected.Flags = append(expected.Flags, operation.flag)
		} else {
			flags := []imap.Flag{}
			for _, flag := range expected.Flags {
				if flag != operation.flag {
					flags = append(flags, flag)
				}
			}
			expected.Flags = flags
		}
		sort.Slice(expected.Flags, func(i, j int) bool { return expected.Flags[i] < expected.Flags[j] })
		before["INBOX"][imap.UID(ref.UID)] = expected
		assertAccountSnapshot(t, before, after)
	}
	for _, operation := range []struct{ name, id string }{
		{"copy_messages", "page-01"}, {"move_messages", "page-02"}, {"trash_messages", "page-03"},
	} {
		before := fixtureSnapshots(t, f, boxes)
		ref := refs[uint32(f.UIDs[operation.id])]
		args := map[string]any{"refs": []mail.MessageRef{ref}}
		destination := "Archive"
		if operation.name == "trash_messages" {
			destination = "MCP-Trash"
		} else {
			args["destination"] = destination
		}
		result := mcpData[mail.MutationResult](t, p, operation.name, args)
		if !result.MappingAvailable || result.OutcomeUnknown || len(result.DestinationRefs) != 1 ||
			!reflect.DeepEqual(result.Affected, []mail.MessageRef{ref}) {
			t.Fatalf("%s did not return authoritative UIDPLUS mapping: %+v", operation.name, result)
		}
		dest := result.DestinationRefs[0]
		status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{"mailbox": destination})
		if dest.Mailbox != destination || dest.UID == 0 || dest.UIDValidity != status.UIDValidity {
			t.Fatalf("%s destination reference disagrees with server: %+v / %+v", operation.name, dest, status)
		}
		source := before["INBOX"][imap.UID(ref.UID)]
		if operation.name != "copy_messages" {
			delete(before["INBOX"], imap.UID(ref.UID))
		}
		if before[destination] == nil {
			before[destination] = map[imap.UID]observedMessage{}
			boxes = append(boxes, destination)
		}
		source.UID = imap.UID(dest.UID)
		after := fixtureSnapshots(t, f, boxes)
		before[destination][imap.UID(dest.UID)] = source
		if sha256.Sum256([]byte(after[destination][imap.UID(dest.UID)].Raw)) != sha256.Sum256([]byte(f.Seeds[operation.id].Raw)) {
			t.Fatalf("%s changed destination raw bytes", operation.name)
		}
		assertAccountSnapshot(t, before, after)
		if operation.name == "trash_messages" {
			assertMCPError(t, p, "trash_messages", map[string]any{"refs": []mail.MessageRef{dest}}, "invalid_argument")
			assertAccountSnapshot(t, after, fixtureSnapshots(t, f, boxes))
		}
	}
	before := fixtureSnapshots(t, f, boxes)
	// Invalid writes must not change any mailbox, including the Deleted sentinel.
	for _, tool := range []string{"add_flags", "remove_flags", "copy_messages", "move_messages", "trash_messages"} {
		stale := refs[uint32(f.UIDs["unseen"])]
		stale.UIDValidity++
		args := map[string]any{"refs": []mail.MessageRef{stale}}
		switch tool {
		case "add_flags", "remove_flags":
			args["flags"] = []string{`\Seen`}
		case "copy_messages", "move_messages":
			args["destination"] = "Archive"
		}
		assertMCPError(t, p, tool, args, "stale_reference")
	}
	assertMCPError(t, p, "add_flags", map[string]any{
		"refs": []mail.MessageRef{refs[uint32(f.UIDs["unseen"])]}, "flags": []string{`\Deleted`},
	}, "invalid_argument")
	raw := "From: local@example.test\r\nTo: recipient@example.test\r\nSubject: Appended local fixture\r\nMessage-ID: <mcp-append@fixture.test>\r\n\r\nStored via IMAP APPEND.\r\n"
	assertMCPError(t, p, "append_message", map[string]any{
		"message_base64": base64.StdEncoding.EncodeToString([]byte(raw)), "flags": []string{`\Deleted`},
	}, "invalid_argument")
	assertAccountSnapshot(t, before, fixtureSnapshots(t, f, boxes))
	internalDate := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	appended := mcpData[mail.MutationResult](t, p, "append_message", map[string]any{
		"message_base64": base64.StdEncoding.EncodeToString([]byte(raw)),
		"flags":          []string{`\Draft`}, "internal_date": internalDate.Format(time.RFC3339),
	})
	if !appended.MappingAvailable || appended.OutcomeUnknown || len(appended.DestinationRefs) != 1 {
		t.Fatalf("APPEND did not return authoritative UIDPLUS identity: %+v", appended)
	}
	ref := appended.DestinationRefs[0]
	status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{})
	if ref.Mailbox != "INBOX" || ref.UIDValidity != status.UIDValidity || ref.UID == 0 {
		t.Fatalf("APPEND reference disagrees with server: %+v / %+v", ref, status)
	}
	after := fixtureSnapshots(t, f, boxes)
	before["INBOX"][imap.UID(ref.UID)] = observedMessage{
		UID: imap.UID(ref.UID), Raw: raw,
		Flags:        []imap.Flag{imap.FlagDraft},
		InternalDate: internalDate,
	}
	assertAccountSnapshot(t, before, after)
	if !containsFlag(after["INBOX"][f.UIDs["deleted"]].Flags, imap.FlagDeleted) {
		t.Fatal("already Deleted sentinel did not survive the mutation suite")
	}
}

func persistentFlags(flags []imap.Flag) []string {
	// Rev1 Recent belongs to a session, not stored message state. See testbed/README.md.
	result := []string{}
	for _, flag := range flags {
		if !strings.EqualFold(string(flag), `\Recent`) {
			result = append(result, strings.ToLower(string(flag)))
		}
	}
	sort.Strings(result)
	return result
}

func assertAccountSnapshot(t *testing.T, want, got map[string]map[imap.UID]observedMessage) {
	t.Helper()
	if err := compareAccountSnapshots(want, got); err != nil {
		t.Fatal(err)
	}
}

func compareAccountSnapshots(want, got map[string]map[imap.UID]observedMessage) error {
	for mailbox, expected := range want {
		actual, exists := got[mailbox]
		if !exists {
			return fmt.Errorf("missing mailbox %q", mailbox)
		}
		for uid, message := range expected {
			observed, exists := actual[uid]
			if !exists {
				return fmt.Errorf("mailbox %q missing UID %d", mailbox, uid)
			}
			if message.UID != observed.UID || observed.UID != uid || message.Raw != observed.Raw ||
				!message.InternalDate.Equal(observed.InternalDate) ||
				!reflect.DeepEqual(persistentFlags(message.Flags), persistentFlags(observed.Flags)) {
				return fmt.Errorf("mailbox %q UID %d: expected UID=%d flags=%v hash=%x date=%v, actual UID=%d flags=%v hash=%x date=%v",
					mailbox, uid, message.UID, message.Flags, sha256.Sum256([]byte(message.Raw)), message.InternalDate,
					observed.UID, observed.Flags, sha256.Sum256([]byte(observed.Raw)), observed.InternalDate)
			}
		}
		for uid := range actual {
			if _, exists := expected[uid]; !exists {
				return fmt.Errorf("mailbox %q contains unexpected UID %d", mailbox, uid)
			}
		}
	}
	for mailbox := range got {
		if _, exists := want[mailbox]; !exists {
			return fmt.Errorf("unexpected mailbox %q", mailbox)
		}
	}
	return nil
}

func TestAccountSnapshotComparison(t *testing.T) {
	date := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	snapshot := func() map[string]map[imap.UID]observedMessage {
		return map[string]map[imap.UID]observedMessage{
			"INBOX":   {1: {UID: 1, Raw: "Subject: fixture\r\n\r\nbody\r\n", Flags: []imap.Flag{imap.FlagDeleted, "fixture-keyword"}, InternalDate: date}},
			"Archive": {},
		}
	}
	tests := []struct {
		name   string
		change func(map[string]map[imap.UID]observedMessage)
		reject bool
	}{
		{"same instant in fixed zone", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.InternalDate = date.In(time.FixedZone("IMAP +0000", 0))
			s["INBOX"][1] = m
		}, false},
		{"transient Recent and flag casing", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.Flags = []imap.Flag{"FIXTURE-KEYWORD", `\Recent`, `\deleted`}
			s["INBOX"][1] = m
		}, false},
		{"missing empty mailbox", func(s map[string]map[imap.UID]observedMessage) { delete(s, "Archive") }, true},
		{"unexpected empty mailbox", func(s map[string]map[imap.UID]observedMessage) { s["Extra"] = map[imap.UID]observedMessage{} }, true},
		{"missing UID", func(s map[string]map[imap.UID]observedMessage) { delete(s["INBOX"], 1) }, true},
		{"unexpected UID", func(s map[string]map[imap.UID]observedMessage) { s["INBOX"][2] = observedMessage{UID: 2} }, true},
		{"changed message UID", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.UID = 2
			s["INBOX"][1] = m
		}, true},
		{"changed raw bytes", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.Raw += "\r\n"
			s["INBOX"][1] = m
		}, true},
		{"Seen added", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.Flags = append(m.Flags, imap.FlagSeen)
			s["INBOX"][1] = m
		}, true},
		{"Deleted sentinel cleared", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.Flags = []imap.Flag{"fixture-keyword"}
			s["INBOX"][1] = m
		}, true},
		{"keyword removed", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.Flags = []imap.Flag{imap.FlagDeleted}
			s["INBOX"][1] = m
		}, true},
		{"changed instant", func(s map[string]map[imap.UID]observedMessage) {
			m := s["INBOX"][1]
			m.InternalDate = date.Add(time.Second)
			s["INBOX"][1] = m
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want, got := snapshot(), snapshot()
			test.change(got)
			if err := compareAccountSnapshots(want, got); (err != nil) != test.reject {
				t.Fatalf("comparison error = %v, want rejection = %t", err, test.reject)
			}
		})
	}
}

func testMCPCacheRefresh(t *testing.T, f *dovecotFixture, binary string) {
	t.Helper()
	configFile, cachePath := mcpFixtureConfig(t, f, "tls", true, "Trash")
	p := startMCP(t, binary, configFile)
	warm := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"limit": 200})
	p.close(t)
	assertCacheReleased(t, cachePath)
	db, err := sql.Open("sqlite", cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted int
	queryErr := db.QueryRow("SELECT COUNT(*) FROM messages WHERE mailbox = 'INBOX'").Scan(&persisted)
	closeErr := db.Close()
	if queryErr != nil || closeErr != nil || persisted != len(warm.Messages) {
		t.Fatalf("header cache not persisted across process exit: rows=%d want=%d query=%v close=%v", persisted, len(warm.Messages), queryErr, closeErr)
	}
	// A separate authenticated peer changes flags, adds mail, and removes a UID.
	c := f.Dial(t, "tls")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imap.UIDSetNum(f.UIDs["flagged"]), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen, "ExTeRnAl-KeyWord"},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	raw := "From: new@example.test\r\nTo: fixture@example.test\r\nSubject: Fresh external metadata\r\nMessage-ID: <external-new@fixture.test>\r\n\r\nFresh external body.\r\n"
	cmd := c.Append("INBOX", int64(len(raw)), nil)
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	appended, err := cmd.Wait()
	if err != nil || appended.UID == 0 {
		t.Fatalf("external APPEND: %+v, %v", appended, err)
	}
	if _, err := c.Move(imap.UIDSetNum(f.UIDs["page-04"]), "Archive").Wait(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	before := fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"})
	p = startMCP(t, binary, configFile)
	fresh := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"limit": 200})
	if fresh.Total != len(before["INBOX"]) || len(fresh.Messages) != len(before["INBOX"]) {
		t.Fatalf("restart did not reconcile complete UID membership: %+v", fresh)
	}
	seenNew := false
	for _, summary := range fresh.Messages {
		observed, exists := before["INBOX"][imap.UID(summary.Ref.UID)]
		if !exists || summary.Ref.UIDValidity != fresh.UIDValidity {
			t.Fatalf("cache retained removed UID or wrong epoch: %+v", summary)
		}
		assertStringFlags(t, summary.Flags, observed.Flags)
		if summary.Ref.UID == uint32(appended.UID) {
			seenNew = summary.Subject == "Fresh external metadata" && len(summary.From) == 1 &&
				strings.Contains(summary.From[0], "new@example.test") && summary.Size == int64(len(raw))
		}
	}
	if !seenNew {
		t.Fatal("new UID metadata was not fetched after restart")
	}
	for _, body := range []bool{false, true} {
		args := map[string]any{"flags": []string{"eXtErNaL-kEyWoRd"}}
		if body {
			args["body"] = "Fixture"
		}
		found := mcpData[mail.MessagePage](t, p, "search_messages", args)
		assertPageUIDs(t, found, f.UIDs["flagged"])
		if !containsCapability(found.Messages[0].Flags, `\Seen`) {
			t.Fatal("CONDSTORE refresh missed external Seen change")
		}
	}
	found := mcpData[mail.MessagePage](t, p, "search_messages", map[string]any{"subject": "Fresh external metadata"})
	assertPageUIDs(t, found, appended.UID)
	assertAccountSnapshot(t, before, fixtureSnapshots(t, f, []string{"INBOX", "Archive", "Trash", "旅行"}))
	docker(t, "stop", "--time", "1", f.Container)
	// The same successful query must fail after loss of its live IMAP server.
	assertMCPError(t, p, "search_messages", map[string]any{"subject": "Fresh external metadata"}, "")
	assertMCPError(t, p, "list_messages", map[string]any{"limit": 200}, "")
	p.close(t)
	assertCacheReleased(t, cachePath)
}
