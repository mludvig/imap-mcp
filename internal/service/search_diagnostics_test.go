package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestSearchLogsUnderlyingFailureBeforeSanitizing(t *testing.T) {
	const password = "password"
	const mailbox = "Archive password mailbox-context"
	const rawCause = "underlying backend failure password private-host private-user"
	for _, tc := range []struct {
		stage, closeAt, code, cause string
		live                        bool
	}{
		{"cache state", "Reading cached mailbox state", "cache_error", "sql: database is closed", false},
		{"backend refresh", "", "server_error", rawCause, false},
		{"refresh cache apply", "Applying refreshed mailbox headers", "cache_error", "sql: database is closed", false},
		{"local header query", "Searching local mailbox headers", "cache_error", "sql: database is closed", false},
		{"backend search", "", "server_error", rawCause, true},
		{"mailbox epoch check", "", "server_error", rawCause, true},
		{"cache result lookup", "Reading matching cached message headers", "cache_error", "sql: database is closed", true},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			b := &fakeBackend{
				refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
					if tc.stage == "backend refresh" {
						return mail.Refresh{}, errors.New(rawCause)
					}
					return refreshData(1, []uint32{1}, "message"), nil
				},
				search: func(context.Context, mail.SearchQuery) ([]uint32, error) {
					if tc.stage == "backend search" {
						return nil, errors.New(rawCause)
					}
					return []uint32{1}, nil
				},
				status: func(context.Context, string) (mail.MailboxStatus, error) {
					if tc.stage == "mailbox epoch check" {
						return mail.MailboxStatus{}, errors.New(rawCause)
					}
					return mail.MailboxStatus{UIDValidity: 1}, nil
				},
			}
			s := newService(t, true, b)
			sink, err := diagnostics.New(t.TempDir(), password, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			ctx := diagnostics.WithProgress(diagnostics.WithLogger(context.Background(), sink.Logger()), func(stage string) {
				if stage == tc.closeAt {
					if err := s.store.Close(); err != nil {
						t.Fatal(err)
					}
				}
			})
			q := mail.SearchQuery{Mailbox: mailbox}
			if tc.live {
				q.Body = "message"
			}
			_, err = s.SearchMessages(ctx, q)
			public := expectCode(t, err, tc.code)
			if strings.Contains(public.Message, "underlying") || strings.Contains(public.Message, "sql:") || strings.Contains(public.Message, password) {
				t.Fatalf("underlying cause exposed publicly: %q", public.Message)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(sink.Path())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), password) {
				t.Fatal("password exposed in diagnostics")
			}
			failures := 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["level"] != "ERROR" {
					continue
				}
				failures++
				wantCause := strings.ReplaceAll(tc.cause, password, "[REDACTED]")
				if record["error"] != wantCause {
					t.Fatalf("underlying cause missing: %+v", record)
				}
				if record["stage"] != tc.stage || record["host"] != s.cfg.IMAP().Host() || record["username"] != s.cfg.IMAP().Username() || record["mailbox"] != strings.ReplaceAll(mailbox, password, "[REDACTED]") {
					t.Fatalf("mailbox context missing: %+v", record)
				}
			}
			if failures != 1 {
				t.Fatalf("error records=%d, want one: %s", failures, data)
			}
		})
	}
}

func TestSearchDoesNotLogAlreadySanitizedBackendFailureAgain(t *testing.T) {
	failure := mail.NewError("server_error", "The IMAP operation failed")
	b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		return mail.Refresh{}, failure
	}}
	s := newService(t, true, b)
	sink, err := diagnostics.New(t.TempDir(), s.cfg.IMAP().Password(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	ctx := diagnostics.WithLogger(context.Background(), sink.Logger())
	if _, err := s.SearchMessages(ctx, mail.SearchQuery{}); err != failure {
		t.Fatalf("typed backend failure changed: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("already logged backend failure logged again: %s", data)
	}
}
