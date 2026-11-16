//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestMCPThreadContract(t *testing.T) {
	binary := buildMCPBinary(t)
	for _, rev2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("rev2=%t", rev2), func(t *testing.T) {
			f := startDovecot(t, rev2)
			c := f.Dial(t, "tls")
			const mailbox = "AuditThreads"
			if err := c.Create(mailbox, nil).Wait(); err != nil {
				t.Fatal(err)
			}
			expectedUIDs := make([]uint32, 0, 201)
			var narrowedUID uint32
			for i := 0; i < 201; i++ {
				header := "ordinary"
				if i == 100 {
					header = "selected"
				}
				raw := fmt.Sprintf("From: audit@example.test\r\nTo: fixture@example.test\r\n"+
					"Subject: Independent audit thread %03d\r\n"+
					"Message-ID: <independent-thread-%03d@fixture.test>\r\n"+
					"X-Thread-Audit: %s\r\nDate: Wed, 15 Jan 2025 12:00:00 +0000\r\n\r\n"+
					"Independent thread body %03d.\r\n", i, i, header, i)
				data := threadObserverAppend(t, c, mailbox, raw)
				expectedUIDs = append(expectedUIDs, uint32(data.UID))
				if header == "selected" {
					narrowedUID = uint32(data.UID)
				}
			}
			c.Close()
			before := f.Snapshot(t, mailbox)
			if len(before) != 201 {
				t.Fatalf("independent THREAD seed count: got %d, want 201", len(before))
			}
			configFile, _ := mcpFixtureConfig(t, f, "tls", true, "Trash")
			p := startMCP(t, binary, configFile)
			assertThreadCriteriaSchema(t, p)
			for field, value := range map[string]any{"limit": 10, "sort": "subject", "before_uid": 1} {
				t.Run("reject-"+field, func(t *testing.T) {
					result := p.call(t, "thread_messages", map[string]any{
						"mailbox": mailbox, "query": map[string]any{field: value},
					})
					if !result.IsError {
						t.Fatalf("THREAD accepted non-criteria query field %q: %+v", field, result.StructuredContent)
					}
				})
			}
			info := mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
			status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{"mailbox": mailbox})
			if containsCapability(info.Capabilities, "THREAD=REFERENCES") {
				result := mcpData[mail.ThreadResult](t, p, "thread_messages", map[string]any{"mailbox": mailbox})
				gotUIDs := threadContractUIDs(t, result.Threads, mailbox, status.UIDValidity)
				sort.Slice(expectedUIDs, func(i, j int) bool { return expectedUIDs[i] < expectedUIDs[j] })
				if len(result.Threads) != 201 || !reflect.DeepEqual(gotUIDs, expectedUIDs) {
					t.Fatalf("default THREAD omitted independent messages: roots=%d, got=%v, want=%v",
						len(result.Threads), gotUIDs, expectedUIDs)
				}
				narrowed := mcpData[mail.ThreadResult](t, p, "thread_messages", map[string]any{
					"mailbox": mailbox, "query": map[string]any{"headers": map[string]string{"X-Thread-Audit": "selected"}},
				})
				if got := threadContractUIDs(t, narrowed.Threads, mailbox, status.UIDValidity); !reflect.DeepEqual(got, []uint32{narrowedUID}) {
					t.Fatalf("THREAD header criteria did not narrow: got %v, want %d", got, narrowedUID)
				}
			} else {
				assertMCPError(t, p, "thread_messages", map[string]any{"mailbox": mailbox}, "unsupported")
			}
			if !containsCapability(info.Capabilities, "THREAD=ORDEREDSUBJECT") {
				assertMCPError(t, p, "thread_messages", map[string]any{
					"mailbox": mailbox, "algorithm": "ORDEREDSUBJECT",
				}, "unsupported")
			}
			assertAccountSnapshot(t, map[string]map[imap.UID]observedMessage{mailbox: before},
				map[string]map[imap.UID]observedMessage{mailbox: f.Snapshot(t, mailbox)})
		})
	}
}

func threadObserverAppend(t *testing.T, c *imapclient.Client, mailbox, raw string) *imap.AppendData {
	t.Helper()
	cmd := c.Append(mailbox, int64(len(raw)), nil)
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil || data == nil || data.UID == 0 || data.UIDValidity == 0 {
		t.Fatalf("independent THREAD seed APPEND identity: %+v (%v)", data, err)
	}
	return data
}

func threadContractUIDs(t *testing.T, nodes []mail.ThreadNode, mailbox string, epoch uint32) []uint32 {
	t.Helper()
	uids := []uint32{}
	seen := make(map[uint32]bool)
	var walk func([]mail.ThreadNode)
	walk = func(nodes []mail.ThreadNode) {
		for _, node := range nodes {
			if node.Ref.Mailbox != mailbox || node.Ref.UIDValidity != epoch || node.Ref.UID == 0 || seen[node.Ref.UID] {
				t.Fatalf("THREAD invalid or duplicated identity: %+v", node.Ref)
			}
			seen[node.Ref.UID] = true
			uids = append(uids, node.Ref.UID)
			walk(node.Children)
		}
	}
	walk(nodes)
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

func assertThreadCriteriaSchema(t *testing.T, p *mcpProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := p.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "thread_messages" {
			continue
		}
		data, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		query := schema.Properties["query"].Properties
		if len(query) == 0 {
			t.Fatalf("THREAD schema lacks criteria: %s", data)
		}
		for _, field := range []string{"limit", "sort", "before_uid"} {
			if _, exists := query[field]; exists {
				t.Errorf("THREAD schema advertises non-criteria field %q: %s", field, data)
			}
		}
		if _, exists := query["headers"]; !exists {
			t.Errorf("THREAD schema omitted header criteria: %s", data)
		}
		return
	}
	t.Fatal("thread_messages absent from MCP tools/list")
}
