package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestSearchReportsSynchronizationAndQueryPhases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query mail.SearchQuery
		live  bool
	}{
		{"local headers", mail.SearchQuery{Subject: "message", Limit: 1}, false},
		{"remote body", mail.SearchQuery{Body: "message", Limit: 1}, true},
		{"remote recipient sort", mail.SearchQuery{Sort: "to", Limit: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stages []string
			b := &fakeBackend{
				refresh: func(ctx context.Context, _ string, state mail.CacheState) (mail.Refresh, error) {
					if len(stages) == 0 || stages[len(stages)-1] != "Synchronizing mailbox headers; first synchronization may take longer" {
						t.Fatal("backend refresh started before synchronization progress")
					}
					// Backends receive the per-call progress context for fetch stages.
					diagnostics.Stage(ctx, "Fetching message headers")
					return refreshData(1, []uint32{1, 2, 3}, "message"), nil
				},
				search: func(context.Context, mail.SearchQuery) ([]uint32, error) {
					return []uint32{3, 2, 1}, nil
				},
			}
			s := newService(t, true, b)
			sink, err := diagnostics.New(t.TempDir(), "", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sink.Close() })
			ctx := diagnostics.WithProgress(diagnostics.WithLogger(context.Background(), sink.Logger()), func(message string) {
				stages = append(stages, message)
			})
			page, err := s.SearchMessages(ctx, tc.query)
			if err != nil || page.Total != 3 || len(page.Messages) != 1 {
				t.Fatalf("page=%+v error=%v", page, err)
			}
			want := []string{
				"Waiting for mailbox access", "Reading cached mailbox state",
				"Synchronizing mailbox headers; first synchronization may take longer",
				"Fetching message headers", "Applying refreshed mailbox headers",
			}
			durations := []string{"mailbox access wait", "cache state", "backend refresh", "refresh cache apply", "mailbox refresh"}
			if tc.live {
				want = append(want, "Searching the IMAP server", "Checking mailbox UID epoch", "Reading matching cached message headers", "Checking mailbox UID epoch")
				durations = append(durations, "backend search", "mailbox epoch check", "cache result lookup", "live search page")
			} else {
				want = append(want, "Searching local mailbox headers")
				durations = append(durations, "local header query")
			}
			next := 0
			for _, stage := range stages {
				if next < len(want) && stage == want[next] {
					next++
				}
			}
			if next != len(want) {
				t.Fatalf("required phase order missing: stages=%v, want %v", stages, want)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(sink.Path())
			if err != nil {
				t.Fatal(err)
			}
			measured := make(map[string]int)
			for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] == "duration" {
					measured[record["stage"].(string)]++
					if record["duration"].(float64) < 0 {
						t.Fatal("negative duration")
					}
				}
			}
			for _, stage := range durations {
				wantCount := 1
				if stage == "mailbox epoch check" {
					wantCount = 2
				}
				if measured[stage] != wantCount {
					t.Fatalf("duration %q count=%d, want %d", stage, measured[stage], wantCount)
				}
			}
			if !tc.live && b.count("search") != 0 || tc.live && b.count("search") != 1 {
				t.Fatal("progress changed local/remote routing")
			}
		})
	}
}

func TestSearchProgressStopsAtFailureAndKeepsValidationGates(t *testing.T) {
	b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		return mail.Refresh{}, errors.New("private backend failure")
	}}
	s := newService(t, true, b)
	var stages []string
	ctx := diagnostics.WithProgress(context.Background(), func(message string) { stages = append(stages, message) })
	_, err := s.SearchMessages(ctx, mail.SearchQuery{Limit: mail.MaxLimit + 1})
	expectCode(t, err, "invalid_argument")
	if len(stages) != 0 || b.count("refresh") != 0 {
		t.Fatal("invalid query entered synchronization")
	}
	_, err = s.SearchMessages(ctx, mail.SearchQuery{})
	expectCode(t, err, "server_error")
	want := []string{
		"Waiting for mailbox access", "Reading cached mailbox state",
		"Synchronizing mailbox headers; first synchronization may take longer",
	}
	if !reflect.DeepEqual(stages, want) || strings.Contains(err.Error(), "private backend failure") {
		t.Fatalf("stages=%v; error sanitization preserved=%t", stages, !strings.Contains(err.Error(), "private backend failure"))
	}
	stages = nil
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.SearchMessages(canceled, mail.SearchQuery{})
	if !errors.Is(err, context.Canceled) || b.count("refresh") != 1 {
		t.Fatal("canceled operation entered backend refresh")
	}
	if !reflect.DeepEqual(stages, []string{"Waiting for mailbox access"}) {
		t.Fatalf("canceled search stages=%v", stages)
	}
}
