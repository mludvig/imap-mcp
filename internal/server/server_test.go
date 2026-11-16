package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/mludvig/imap-mcp/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type testBackend struct {
	mail.Backend
	infoError error
	read      mail.ReadRequest
	thread    mail.ThreadRequest
	threads   []mail.ThreadNode
	appended  mail.AppendRequest
}

func (b *testBackend) Info(context.Context) (mail.ServerInfo, error) {
	return mail.ServerInfo{Capabilities: []string{"IMAP4rev1"}}, b.infoError
}
func (b *testBackend) Refresh(context.Context, string, mail.CacheState) (mail.Refresh, error) {
	return mail.Refresh{UIDValidity: 7, Members: []uint32{}}, nil
}
func (b *testBackend) ReadMessage(_ context.Context, req mail.ReadRequest) (mail.Message, error) {
	b.read = req
	return mail.Message{Summary: mail.MessageSummary{Ref: req.Ref, InternalDate: time.Now()}, Text: "mail"}, nil
}
func (b *testBackend) ThreadMessages(_ context.Context, req mail.ThreadRequest) (mail.ThreadResult, error) {
	b.thread = req
	if b.threads != nil {
		return mail.ThreadResult{Threads: b.threads}, nil
	}
	return mail.ThreadResult{Threads: []mail.ThreadNode{{
		Ref:      mail.MessageRef{Mailbox: req.Mailbox, UIDValidity: 7, UID: 1},
		Children: []mail.ThreadNode{{Ref: mail.MessageRef{Mailbox: req.Mailbox, UIDValidity: 7, UID: 2}}},
	}}}, nil
}
func (b *testBackend) AppendMessage(_ context.Context, req mail.AppendRequest) (mail.MutationResult, error) {
	b.appended = req
	return mail.MutationResult{Affected: []mail.MessageRef{}, DestinationRefs: []mail.MessageRef{}}, nil
}

func fixture(t *testing.T, readonly bool) (*mcp.Server, *testBackend, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := fmt.Sprintf("readonly = %t\n[imap]\nhost = \"example.test\"\nusername = \"test\"\npassword = \"secret\"\n[cache]\npath = \"headers.sqlite3\"\n", readonly)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.Open(cfg.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	backend := &testBackend{}
	return New(service.New(cfg, backend, store)), backend, path
}

func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func tools(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	result, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]*mcp.Tool)
	for _, tool := range result.Tools {
		found[tool.Name] = tool
	}
	return found
}

func schema(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func required(t *testing.T, s map[string]any, want ...string) {
	t.Helper()
	var got []string
	for _, name := range array(s["required"]) {
		got = append(got, name.(string))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
}

func array(value any) []any {
	if value == nil {
		return nil
	}
	return value.([]any)
}

func TestInventorySchemasAndImmutableMode(t *testing.T) {
	readNames := []string{"server_info", "list_mailboxes", "mailbox_status", "list_messages", "search_messages", "read_message", "get_attachment", "thread_messages"}
	writeNames := []string{"add_flags", "remove_flags", "copy_messages", "move_messages", "trash_messages", "append_message"}
	for _, readonly := range []bool{true, false} {
		t.Run(fmt.Sprint(readonly), func(t *testing.T) {
			server, _, path := fixture(t, readonly)
			cs := connect(t, server)
			if cs.InitializeResult().Capabilities.Tools.ListChanged {
				t.Fatal("immutable inventory advertised listChanged")
			}
			found := tools(t, cs)
			wantCount := 14
			if readonly {
				wantCount = 8
			}
			if len(found) != wantCount {
				t.Fatalf("inventory = %v", found)
			}
			for _, name := range readNames {
				tool := found[name]
				if tool == nil || !tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint || !*tool.Annotations.OpenWorldHint {
					t.Fatalf("invalid read annotations for %s: %+v", name, tool)
				}
			}
			for _, name := range writeNames {
				tool := found[name]
				if readonly {
					if tool != nil {
						t.Fatalf("readonly exposes %s", name)
					}
					continue
				}
				wantDestructive := name == "remove_flags" || name == "move_messages" || name == "trash_messages"
				if tool == nil || tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint != wantDestructive || !*tool.Annotations.OpenWorldHint {
					t.Fatalf("invalid write annotations for %s: %+v", name, tool)
				}
			}
			for _, name := range []string{"server_info", "list_mailboxes", "mailbox_status", "list_messages", "search_messages", "thread_messages"} {
				required(t, schema(t, found[name].InputSchema))
			}
			required(t, schema(t, found["read_message"].InputSchema), "ref")
			readSchema := schema(t, found["read_message"].InputSchema)
			refSchema := readSchema["properties"].(map[string]any)["ref"].(map[string]any)
			required(t, refSchema, "mailbox", "uid_validity", "uid")
			required(t, schema(t, found["get_attachment"].InputSchema), "ref", "part_id")
			searchProperties := schema(t, found["search_messages"].InputSchema)["properties"].(map[string]any)
			if len(searchProperties) != 20 {
				t.Fatalf("search fields = %v", searchProperties)
			}
			if !readonly {
				required(t, schema(t, found["append_message"].InputSchema), "message_base64")
				required(t, schema(t, found["trash_messages"].InputSchema), "refs")
				required(t, schema(t, found["add_flags"].InputSchema), "refs", "flags")
				required(t, schema(t, found["copy_messages"].InputSchema), "refs", "destination")
			}
			if err := os.WriteFile(path, []byte("readonly = false\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if len(tools(t, cs)) != wantCount {
				t.Fatal("config file edit changed registry")
			}
		})
	}
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMinimalCallsAndDomainErrors(t *testing.T) {
	server, backend, _ := fixture(t, false)
	cs := connect(t, server)
	for _, name := range []string{"search_messages", "list_messages"} {
		if res := call(t, cs, name, map[string]any{}); res.IsError {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	ref := mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: 1}
	if res := call(t, cs, "read_message", map[string]any{"ref": ref}); res.IsError {
		t.Fatalf("read: %+v", res)
	}
	if backend.read.IncludeHTML || backend.read.MaxTextChars != mail.DefaultTextChars {
		t.Fatalf("read defaults = %+v", backend.read)
	}
	res := call(t, cs, "thread_messages", map[string]any{"mailbox": "Archive"})
	if res.IsError {
		t.Fatalf("thread: %+v", res)
	}
	threads := schema(t, res.StructuredContent)["data"].(map[string]any)["threads"].([]any)
	children := threads[0].(map[string]any)["children"].([]any)
	if len(children) != 1 || children[0].(map[string]any)["ref"].(map[string]any)["uid"] != float64(2) {
		t.Fatalf("recursive thread response = %v", threads)
	}
	if backend.thread.Algorithm != "REFERENCES" || backend.thread.Query.Mailbox != "Archive" || backend.thread.Query.Limit != 0 || backend.thread.Query.BeforeUID != 0 || backend.thread.Query.Sort != "" {
		t.Fatalf("thread defaults = %+v", backend.thread)
	}
	raw := base64.StdEncoding.EncodeToString([]byte("Subject: test\r\n\r\nmail\r\n"))
	if res := call(t, cs, "append_message", map[string]any{"message_base64": raw}); res.IsError {
		t.Fatalf("append: %+v", res)
	}
	if backend.appended.Mailbox != "INBOX" || backend.appended.Flags != nil || backend.appended.InternalDate != nil {
		t.Fatalf("append defaults = %+v", backend.appended)
	}
	backend.infoError = mail.NewError("unsupported", "Feature unavailable")
	res = call(t, cs, "server_info", map[string]any{})
	if !res.IsError || schema(t, res.StructuredContent)["error"].(map[string]any)["code"] != "unsupported" {
		t.Fatalf("domain result = %+v", res)
	}
	if _, hasData := schema(t, res.StructuredContent)["data"]; hasData {
		t.Fatal("failed result contains partial data")
	}
	for _, args := range []any{map[string]any{}, map[string]any{"ref": map[string]any{"mailbox": "INBOX", "uid": 1}}, map[string]any{"ref": "bad"}} {
		if res := call(t, cs, "read_message", args); !res.IsError {
			t.Fatalf("invalid read accepted: %v", args)
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "permanently_delete", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool did not return protocol error")
	}
}

func TestThreadSchemaExposesCriteriaAndRecursiveTrees(t *testing.T) {
	server, _, _ := fixture(t, true)
	found := tools(t, connect(t, server))
	thread := found["thread_messages"]
	input := schema(t, thread.InputSchema)
	required(t, input)
	query := input["properties"].(map[string]any)["query"].(map[string]any)
	required(t, query)
	properties := query["properties"].(map[string]any)
	want := []string{"mailbox", "from", "to", "cc", "bcc", "subject", "body", "text", "headers", "since", "before", "sent_since", "sent_before", "flags", "not_flags", "min_size", "max_size"}
	var got []string
	for name := range properties {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) || query["additionalProperties"] != false {
		t.Fatalf("THREAD query schema = %v, want only %v with additionalProperties=false", query, want)
	}
	t.Logf("THREAD query properties=%v; additionalProperties=%v; required=%v", got, query["additionalProperties"], query["required"])
	for _, name := range []string{"limit", "before_uid", "sort"} {
		search := schema(t, found["search_messages"].InputSchema)["properties"].(map[string]any)
		list := schema(t, found["list_messages"].InputSchema)["properties"].(map[string]any)
		if search[name] == nil || list[name] == nil {
			t.Fatalf("%s removed from search/list schema", name)
		}
	}
	if !strings.Contains(thread.Description, "complete") || !strings.Contains(thread.Description, "criteria") || strings.Contains(thread.Description, "limit defaults") {
		t.Fatalf("THREAD description = %q", thread.Description)
	}
	output := schema(t, thread.OutputSchema)
	node := output["$defs"].(map[string]any)["thread_node"].(map[string]any)
	children := node["properties"].(map[string]any)["children"].(map[string]any)["items"].(map[string]any)
	threads := output["properties"].(map[string]any)["data"].(map[string]any)["properties"].(map[string]any)["threads"].(map[string]any)["items"].(map[string]any)
	if threads["$ref"] != "#/$defs/thread_node" || children["$ref"] != "#/$defs/thread_node" {
		t.Fatalf("recursive THREAD output schema = %v", output)
	}
	t.Logf("THREAD root items $ref=%v; children items $ref=%v", threads["$ref"], children["$ref"])
}

func TestThreadRejectsUnsupportedControlsBeforeDispatch(t *testing.T) {
	server, backend, _ := fixture(t, true)
	cs := connect(t, server)
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"limit", 1}, {"limit", 0}, {"before_uid", 100}, {"before_uid", 0}, {"sort", "uid_desc"}, {"sort", ""},
	} {
		t.Run(fmt.Sprintf("%s=%v", tc.name, tc.value), func(t *testing.T) {
			res := call(t, cs, "thread_messages", map[string]any{"query": map[string]any{tc.name: tc.value}})
			if !res.IsError || len(res.Content) == 0 {
				t.Fatalf("unsupported %s accepted: %+v", tc.name, res)
			}
			message := res.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(message, tc.name) {
				t.Fatalf("validation error does not identify %s: %q", tc.name, message)
			}
			if !reflect.DeepEqual(backend.thread, mail.ThreadRequest{}) {
				t.Fatalf("invalid THREAD dispatched: %+v", backend.thread)
			}
			t.Logf("SDK rejected %s=%v with IsError=true: %s", tc.name, tc.value, message)
		})
	}
}

func TestThreadOmittedCriteriaReturnsCompleteTrees(t *testing.T) {
	for _, name := range []string{"INBOX", "Archive"} {
		t.Run(name, func(t *testing.T) {
			server, backend, _ := fixture(t, true)
			backend.threads = make([]mail.ThreadNode, 201)
			for i := range backend.threads {
				backend.threads[i] = mail.ThreadNode{
					Ref: mail.MessageRef{Mailbox: name, UIDValidity: 7, UID: uint32(3*i + 1)},
					Children: []mail.ThreadNode{{
						Ref: mail.MessageRef{Mailbox: name, UIDValidity: 7, UID: uint32(3*i + 2)},
						Children: []mail.ThreadNode{{
							Ref: mail.MessageRef{Mailbox: name, UIDValidity: 7, UID: uint32(3*i + 3)},
						}},
					}},
				}
			}
			args := map[string]any{}
			if name != "INBOX" {
				args["mailbox"] = name
			}
			res := call(t, connect(t, server), "thread_messages", args)
			if res.IsError {
				t.Fatalf("THREAD result = %+v", res)
			}
			var out envelope[mail.ThreadResult]
			payload, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(payload, &out); err != nil {
				t.Fatal(err)
			}
			if out.Data == nil || !reflect.DeepEqual(out.Data.Threads, backend.threads) {
				t.Fatalf("complete THREAD trees were altered: %+v", out.Data)
			}
			q := backend.thread.Query
			if backend.thread.Mailbox != name || backend.thread.Algorithm != "REFERENCES" || !reflect.DeepEqual(q, mail.SearchQuery{Mailbox: name}) {
				t.Fatalf("omitted THREAD criteria defaults = %+v", backend.thread)
			}
			t.Logf("returned all %d roots with children and grandchildren; backend query=%+v", len(out.Data.Threads), q)
		})
	}
}

func TestThreadCriteriaForwardingAndMailboxValidation(t *testing.T) {
	server, backend, _ := fixture(t, true)
	cs := connect(t, server)
	q := mail.SearchQuery{
		Mailbox: "Archive", From: "sender", To: "recipient", Cc: "copy", Bcc: "blind",
		Subject: "subject", Body: "body", Text: "text", Headers: map[string]string{"X-Test": "value"},
		Since: "2026-01-01", Before: "2026-02-01", SentSince: "2025-12-01", SentBefore: "2026-03-01",
		Flags: []string{"\\Seen"}, NotFlags: []string{"\\Deleted"}, MinSize: 1, MaxSize: 1000,
	}
	args := schema(t, q)
	for _, name := range []string{"limit", "before_uid", "sort"} {
		delete(args, name)
	}
	res := call(t, cs, "thread_messages", map[string]any{"mailbox": "Archive", "algorithm": "orderedsubject", "query": args})
	if res.IsError || !reflect.DeepEqual(backend.thread, mail.ThreadRequest{Mailbox: "Archive", Algorithm: "ORDEREDSUBJECT", Query: q}) {
		t.Fatalf("THREAD criteria forwarding: result=%+v backend=%+v", res, backend.thread)
	}
	backend.thread = mail.ThreadRequest{}
	res = call(t, cs, "thread_messages", map[string]any{"mailbox": "Archive", "query": map[string]any{"mailbox": "Other"}})
	if !res.IsError || schema(t, res.StructuredContent)["error"].(map[string]any)["code"] != "invalid_argument" || !reflect.DeepEqual(backend.thread, mail.ThreadRequest{}) {
		t.Fatalf("mismatched mailbox reached backend: result=%+v backend=%+v", res, backend.thread)
	}
}

func TestThreadOversizedCompleteResultSuggestsNarrowerCriteria(t *testing.T) {
	server, backend, _ := fixture(t, true)
	backend.threads = make([]mail.ThreadNode, 100000)
	for i := range backend.threads {
		backend.threads[i].Ref = mail.MessageRef{Mailbox: "INBOX", UIDValidity: 7, UID: uint32(i + 1)}
	}
	res := call(t, connect(t, server), "thread_messages", map[string]any{})
	out := schema(t, res.StructuredContent)
	if !res.IsError || out["data"] != nil {
		t.Fatalf("oversized THREAD result was not rejected: %+v", out)
	}
	failure := out["error"].(map[string]any)
	message := failure["message"].(string)
	if failure["code"] != "response_too_large" || !strings.Contains(message, "narrow query criteria") || !strings.Contains(message, "THREAD") {
		t.Fatalf("oversized THREAD error = %+v", failure)
	}
	wire, err := json.Marshal(res)
	if err != nil || len(wire) > maxResultBytes {
		t.Fatalf("oversize error response: bytes=%d error=%v", len(wire), err)
	}
	t.Logf("complete %d-root THREAD response rejected with %s; bounded error=%d bytes: %s", len(backend.threads), failure["code"], len(wire), message)
}

func TestResponseLimitCountsEscapedFallbackAndSanitizesErrors(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type output struct {
		Text string `json:"text"`
	}
	add(server, "oversized", "", true, false, func(context.Context, struct{}) (output, error) {
		return output{Text: strings.Repeat("\x00", 700000)}, nil
	})
	add(server, "attachment_size", "", true, false, func(context.Context, struct{}) (output, error) {
		return output{Text: base64.StdEncoding.EncodeToString(make([]byte, mail.MaxAttachmentBytes))}, nil
	})
	add(server, "unknown_error", "", true, false, func(context.Context, struct{}) (output, error) {
		return output{}, errors.New("password secret /private/path SQL transport bytes")
	})
	cs := connect(t, server)
	for name, code := range map[string]string{"oversized": "response_too_large", "unknown_error": "internal_error"} {
		res := call(t, cs, name, map[string]any{})
		wire, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || schema(t, res.StructuredContent)["error"].(map[string]any)["code"] != code || len(wire) > maxResultBytes || strings.Contains(string(wire), "secret") {
			t.Fatalf("%s result = %s", name, wire)
		}
		if name == "oversized" {
			message := schema(t, res.StructuredContent)["error"].(map[string]any)["message"].(string)
			if !strings.Contains(message, "narrow") || !strings.Contains(message, "criteria") {
				t.Fatalf("oversize error lacks guidance for THREAD: %q", message)
			}
		}
	}
	res := call(t, cs, "attachment_size", map[string]any{})
	wire, err := json.Marshal(res)
	if err != nil || res.IsError || len(wire) > maxResultBytes {
		t.Fatalf("maximum attachment response: bytes=%d error=%v IsError=%v", len(wire), err, res.IsError)
	}
}
