package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectObserved(t *testing.T, ctx context.Context, server *mcp.Server, observe func(*mcp.ProgressNotificationParams)) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			observe(req.Params)
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestSearchProgressUsesOnlySuppliedToken(t *testing.T) {
	for _, token := range []any{nil, "search-token", "", 0} {
		t.Run(fmt.Sprintf("%T-%v", token, token), func(t *testing.T) {
			server, _, _ := fixture(t, true)
			var mu sync.Mutex
			var got []mcp.ProgressNotificationParams
			cs := connectObserved(t, context.Background(), server, func(p *mcp.ProgressNotificationParams) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, *p)
			})
			params := &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"limit": 1}}
			if token != nil {
				params.SetProgressToken(token)
			}
			res, err := cs.CallTool(context.Background(), params)
			if err != nil || res.IsError {
				t.Fatalf("search error=%v result=%+v", err, res)
			}
			// Drain queued notification handlers before inspecting progress.
			if err := cs.Close(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if token == nil {
				if len(got) != 0 {
					t.Fatal("call without a token sent progress")
				}
				return
			}
			want := []string{
				"Starting search_messages", "Waiting for mailbox access", "Reading cached mailbox state",
				"Synchronizing mailbox headers; first synchronization may take longer",
				"Applying refreshed mailbox headers", "Searching local mailbox headers", "Finished search_messages",
			}
			next := 0
			for i, p := range got {
				tokenJSON, _ := json.Marshal(token)
				gotTokenJSON, _ := json.Marshal(p.ProgressToken)
				if string(gotTokenJSON) != string(tokenJSON) || p.Progress != float64(i+1) || p.Total != 0 || p.Message == "" {
					t.Fatalf("progress %d=%+v", i, p)
				}
				if next < len(want) && p.Message == want[next] {
					next++
				}
			}
			if next != len(want) {
				t.Fatalf("progress omitted or reordered required phases: received=%v", got)
			}
		})
	}
}

func TestConcurrentStagesStayMonotonicAndRoutePerCall(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		ID string `json:"id"`
	}
	add(server, "concurrent", "", true, false, func(ctx context.Context, in input) (input, error) {
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				diagnostics.Stage(ctx, in.ID)
			}()
		}
		wg.Wait()
		return in, nil
	})
	var mu sync.Mutex
	received := make(map[string][]mcp.ProgressNotificationParams)
	cs := connectObserved(t, context.Background(), server, func(p *mcp.ProgressNotificationParams) {
		mu.Lock()
		defer mu.Unlock()
		token := p.ProgressToken.(string)
		received[token] = append(received[token], *p)
	})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			params := &mcp.CallToolParams{Name: "concurrent", Arguments: input{ID: id}}
			params.SetProgressToken(id)
			res, err := cs.CallTool(context.Background(), params)
			if err == nil && res.IsError {
				err = errors.New("concurrent tool returned an error")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Drain queued notification handlers before inspecting progress.
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"first", "second"} {
		got := received[id]
		if len(got) != 66 {
			t.Fatalf("%s notification count=%d", id, len(got))
		}
		for i, p := range got {
			wantMessage := id
			if i == 0 {
				wantMessage = "Starting concurrent"
			} else if i == 65 {
				wantMessage = "Finished concurrent"
			}
			if p.Progress != float64(i+1) || p.Total != 0 || p.Message != wantMessage {
				t.Fatalf("%s progress %d=%+v", id, i, p)
			}
		}
	}
}

func TestCanceledProgressContextDoesNotNotify(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "canceled"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		ctx, cancel := context.WithCancel(ctx)
		ctx = progressContext(ctx, req)
		cancel()
		diagnostics.Stage(ctx, "Canceled stage")
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	var mu sync.Mutex
	count := 0
	cs := connectObserved(t, context.Background(), server, func(*mcp.ProgressNotificationParams) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})
	params := &mcp.CallToolParams{Name: "canceled", Arguments: map[string]any{}}
	params.SetProgressToken("cancel-token")
	if _, err := cs.CallTool(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatal("canceled context sent progress")
	}
}

func TestToolDiagnosticsRetainDetailsAndPublicErrorSafety(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprint(debug), func(t *testing.T) {
			password := "diagnostic-fixture-password"
			sink, err := diagnostics.New(t.TempDir(), password, debug, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sink.Close() })
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			type detail struct {
				From    string `json:"from"`
				Mailbox string `json:"mailbox"`
			}
			add(server, "details", "", true, false, func(context.Context, detail) (detail, error) {
				return detail{From: "user@example.test", Mailbox: "Archive"}, nil
			})
			add(server, "failure", "", true, false, func(context.Context, struct{}) (detail, error) {
				return detail{}, errors.New("private transport failure " + password)
			})
			cs := connectObserved(t, diagnostics.WithLogger(context.Background(), sink.Logger()), server, func(*mcp.ProgressNotificationParams) {
				t.Error("call without a token sent progress")
			})
			if call(t, cs, "details", detail{From: password, Mailbox: "Archive"}).IsError {
				t.Fatal("details call failed")
			}
			res := call(t, cs, "failure", map[string]any{})
			if !res.IsError || schema(t, res.StructuredContent)["error"].(map[string]any)["code"] != "internal_error" {
				t.Fatal("failure did not retain sanitized public error")
			}
			wire, err := json.Marshal(res)
			if err != nil || strings.Contains(string(wire), password) || strings.Contains(string(wire), "private transport failure") {
				t.Fatal("public result exposed diagnostic details")
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(sink.Path())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), password) {
				t.Fatal("diagnostic password was not redacted")
			}
			completed, inputs, outputs, stages := 0, 0, 0, 0
			for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal("invalid diagnostic JSON")
				}
				switch record["msg"] {
				case "tool completed":
					completed++
					if record["level"] != "INFO" || record["duration"].(float64) < 0 || record["tool"] == nil {
						t.Fatal("incomplete tool completion diagnostics")
					}
					if record["tool"] == "failure" && !strings.Contains(record["error"].(string), "private transport failure") {
						t.Fatal("diagnostic error detail was lost")
					}
				case "tool input":
					inputs++
				case "tool result":
					outputs++
				case "stage":
					stages++
				}
			}
			if completed != 2 || (debug && (inputs != 2 || outputs != 2 || stages != 4)) || (!debug && (inputs != 0 || outputs != 0 || stages != 0)) {
				t.Fatalf("diagnostic event counts: completed=%d inputs=%d outputs=%d stages=%d", completed, inputs, outputs, stages)
			}
			if debug && (!strings.Contains(string(log), "user@example.test") || !strings.Contains(string(log), "Archive")) {
				t.Fatal("non-password tool details were lost")
			}
		})
	}
}

func TestGuidanceExplainsSearchCoverageAndCost(t *testing.T) {
	server, _, _ := fixture(t, true)
	cs := connect(t, server)
	instructions := cs.InitializeResult().Instructions
	for _, text := range []string{"AND", "separate queries", "unchanged criteria", "imports", "folder creation", "FROM", "actual reply", "Negative findings", "limit", "synchronize"} {
		if !strings.Contains(instructions, text) {
			t.Fatalf("instructions lack %q", text)
		}
	}
	found := tools(t, cs)
	for _, text := range []string{"AND", "separate queries", "literal", "Unicode", "BODY/TEXT", "To/Cc", "server-defined", "unchanged", "not synchronization", "first call"} {
		if !strings.Contains(found["search_messages"].Description, text) {
			t.Fatalf("search description lacks %q", text)
		}
	}
	for _, name := range []string{"list_messages", "search_messages"} {
		props := schema(t, found[name].InputSchema)["properties"].(map[string]any)
		limit := props["limit"].(map[string]any)
		if !strings.Contains(limit["description"].(string), "Does not bound header synchronization") {
			t.Fatalf("%s limit schema lacks synchronization guidance", name)
		}
	}
	// The descriptions supplement the existing schema, annotation, and sizing tests.
	if mail.DefaultLimit != 50 || mail.MaxLimit != 200 {
		t.Fatal("page defaults changed")
	}
}
