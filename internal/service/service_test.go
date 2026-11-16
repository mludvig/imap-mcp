package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mludvig/imap-mcp/internal/cache"
	"github.com/mludvig/imap-mcp/internal/config"
	"github.com/mludvig/imap-mcp/internal/mail"
)

type fakeBackend struct {
	mu            sync.Mutex
	calls         []string
	refresh       func(context.Context, string, mail.CacheState) (mail.Refresh, error)
	search        func(context.Context, mail.SearchQuery) ([]uint32, error)
	status        func(context.Context, string) (mail.MailboxStatus, error)
	write         func(context.Context, string) (mail.MutationResult, error)
	info          mail.ServerInfo
	boxes         []mail.MailboxInfo
	createErr     error
	create        func(context.Context, string) error
	readRequest   mail.ReadRequest
	threadRequest mail.ThreadRequest
}

func (b *fakeBackend) record(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, name)
}
func (b *fakeBackend) count(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, call := range b.calls {
		if call == name {
			n++
		}
	}
	return n
}
func (b *fakeBackend) Info(context.Context) (mail.ServerInfo, error) {
	b.record("info")
	return b.info, nil
}
func (b *fakeBackend) ListMailboxes(context.Context) ([]mail.MailboxInfo, error) {
	b.record("list")
	return b.boxes, nil
}
func (b *fakeBackend) MailboxStatus(ctx context.Context, name string) (mail.MailboxStatus, error) {
	b.record("status")
	if b.status != nil {
		return b.status(ctx, name)
	}
	return mail.MailboxStatus{Mailbox: name, UIDValidity: 1}, nil
}
func (b *fakeBackend) Refresh(ctx context.Context, name string, state mail.CacheState) (mail.Refresh, error) {
	b.record("refresh")
	return b.refresh(ctx, name, state)
}
func (b *fakeBackend) SearchUIDs(ctx context.Context, q mail.SearchQuery) ([]uint32, error) {
	b.record("search")
	return b.search(ctx, q)
}
func (b *fakeBackend) ReadMessage(_ context.Context, req mail.ReadRequest) (mail.Message, error) {
	b.record("read")
	b.readRequest = req
	return mail.Message{Summary: mail.MessageSummary{Ref: req.Ref}}, nil
}
func (b *fakeBackend) GetAttachment(context.Context, mail.AttachmentRequest) (mail.Attachment, error) {
	b.record("attachment")
	return mail.Attachment{}, nil
}
func (b *fakeBackend) ThreadMessages(_ context.Context, req mail.ThreadRequest) (mail.ThreadResult, error) {
	b.record("thread")
	b.threadRequest = req
	return mail.ThreadResult{}, nil
}
func (b *fakeBackend) mutation(ctx context.Context, name string) (mail.MutationResult, error) {
	b.record(name)
	if b.write != nil {
		return b.write(ctx, name)
	}
	return mail.MutationResult{}, nil
}
func (b *fakeBackend) AddFlags(ctx context.Context, _ mail.FlagRequest) (mail.MutationResult, error) {
	return b.mutation(ctx, "add")
}
func (b *fakeBackend) RemoveFlags(ctx context.Context, _ mail.FlagRequest) (mail.MutationResult, error) {
	return b.mutation(ctx, "remove")
}
func (b *fakeBackend) CopyMessages(ctx context.Context, _ mail.TransferRequest) (mail.MutationResult, error) {
	return b.mutation(ctx, "copy")
}
func (b *fakeBackend) MoveMessages(ctx context.Context, _ mail.TransferRequest) (mail.MutationResult, error) {
	return b.mutation(ctx, "move")
}
func (b *fakeBackend) AppendMessage(ctx context.Context, _ mail.AppendRequest) (mail.MutationResult, error) {
	return b.mutation(ctx, "append")
}
func (b *fakeBackend) CreateMailbox(ctx context.Context, name string) error {
	b.record("create:" + name)
	if b.create != nil {
		return b.create(ctx, name)
	}
	return b.createErr
}

func testConfig(t *testing.T, readonly bool, host, password, username, mode, trash string, port int) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	data := fmt.Sprintf("readonly = %t\ntrash_mailbox = %q\n[cache]\npath = 'headers.sqlite3'\n[imap]\nhost = %q\nport = %d\nusername = %q\npassword = %q\ntls = %q\n", readonly, trash, host, port, username, password, mode)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newService(t *testing.T, readonly bool, b *fakeBackend) *Service {
	t.Helper()
	cfg := testConfig(t, readonly, "Mail.Example", "password", "user", "tls", "Trash", 993)
	store, err := cache.Open(cfg.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return New(cfg, b, store)
}

func ref(name string, uid uint32) mail.MessageRef {
	return mail.MessageRef{Mailbox: name, UIDValidity: 1, UID: uid}
}
func refreshData(epoch uint32, uids []uint32, subject string) mail.Refresh {
	r := mail.Refresh{UIDValidity: epoch, HighestModSeq: 10, Members: uids}
	for _, uid := range uids {
		r.NewMetadata = append(r.NewMetadata, mail.Metadata{UID: uid, Size: int64(uid + 100), InternalDate: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Header: []byte(fmt.Sprintf("Subject: %s %03d\r\nFrom: sender@example.com\r\nTo: receiver@example.com\r\n\r\n", subject, uid))})
		r.FlagUpdates = append(r.FlagUpdates, mail.FlagUpdate{UID: uid, ModSeq: 10, Flags: []string{"MiXeD"}})
	}
	return r
}

func expectCode(t *testing.T, err error, code string) *mail.Error {
	t.Helper()
	var typed *mail.Error
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
	return typed
}

func TestEverySearchRefreshesAndNeverServesStale(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, true, b)
	n := 0
	b.refresh = func(ctx context.Context, name string, state mail.CacheState) (mail.Refresh, error) {
		n++
		if name != "INBOX" {
			t.Fatalf("mailbox=%q", name)
		}
		if n == 1 {
			if state.Present {
				t.Fatal("unexpected initial cache")
			}
			return refreshData(1, []uint32{1, 2}, "Café"), nil
		}
		if !state.Present || state.HighestModSeq != uint64(n+8) || !reflect.DeepEqual(state.CachedUIDs, []uint32{1, 2}) {
			t.Fatalf("state=%+v", state)
		}
		// This runs while no SQLite transaction is held by service.
		if _, err := s.store.State(ctx, s.key(name)); err != nil {
			t.Fatal(err)
		}
		if n == 3 {
			return mail.Refresh{}, errors.New("secret password and raw server response")
		}
		return mail.Refresh{UIDValidity: 1, HighestModSeq: 11, Members: []uint32{1, 2}}, nil
	}
	for i := 0; i < 2; i++ {
		page, err := s.SearchMessages(context.Background(), mail.SearchQuery{Subject: "CAFE\u0301", Flags: []string{"mixed"}})
		if err != nil || page.Total != 2 || page.Messages[0].Ref.UID != 2 {
			t.Fatalf("page=%+v err=%v", page, err)
		}
	}
	page, err := s.SearchMessages(context.Background(), mail.SearchQuery{})
	expectCode(t, err, "server_error")
	if len(page.Messages) != 0 || strings.Contains(err.Error(), "secret") || b.count("refresh") != 3 {
		t.Fatalf("stale page or unsafe error: %+v %v", page, err)
	}
}

func TestQueryValidationBeforeNetwork(t *testing.T) {
	cases := []mail.SearchQuery{{Limit: -1}, {Limit: 201}, {MinSize: -1}, {MaxSize: -1}, {Since: "2026-1-01"}, {Before: "2026-02-30"}, {SentSince: "not a date"}, {SentBefore: "2026-01-01Z"}, {Flags: []string{"bad flag"}}, {Flags: []string{"bad]flag"}}, {NotFlags: []string{"\\Unknown"}}, {Headers: map[string]string{"x y": ""}}, {Headers: map[string]string{"X:Inject": ""}}, {Headers: map[string]string{"é": ""}}, {Sort: "bad"}, {Sort: "uid_desc_asc"}, {Sort: "date", BeforeUID: 10}, {Sort: "uid_asc", BeforeUID: 10}}
	b := &fakeBackend{}
	s := newService(t, true, b)
	for _, q := range cases {
		_, err := s.SearchMessages(context.Background(), q)
		expectCode(t, err, "invalid_argument")
	}
	_, err := s.ListMessages(context.Background(), mail.SearchQuery{Subject: "filter"})
	expectCode(t, err, "invalid_argument")
	if len(b.calls) != 0 {
		t.Fatalf("backend calls=%v", b.calls)
	}
}

func TestLivePagingCountsAllMatches(t *testing.T) {
	uids := make([]uint32, 350)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		return refreshData(1, uids, "body"), nil
	}}
	b.search = func(_ context.Context, q mail.SearchQuery) ([]uint32, error) {
		if q.BeforeUID != 0 || q.Mailbox != "INBOX" || q.Body != "needle" {
			t.Fatalf("query=%+v", q)
		}
		return uids, nil
	}
	s := newService(t, true, b)
	page, err := s.SearchMessages(context.Background(), mail.SearchQuery{Body: "needle", Limit: 200, BeforeUID: 300})
	if err != nil || page.Total != 350 || len(page.Messages) != 200 || page.Messages[0].Ref.UID != 299 || page.Messages[199].Ref.UID != 100 || page.NextBeforeUID != 100 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = s.SearchMessages(context.Background(), mail.SearchQuery{Body: "needle", Limit: 200, BeforeUID: 100})
	if err != nil || page.Total != 350 || len(page.Messages) != 99 || page.NextBeforeUID != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if b.count("refresh") != 2 || b.count("status") != 4 {
		t.Fatalf("calls=%v", b.calls)
	}
}

func TestLiveSearchRetriesEpochWithoutRebindingUID(t *testing.T) {
	for _, afterLookup := range []bool{false, true} {
		t.Run(fmt.Sprint(afterLookup), func(t *testing.T) {
			b := &fakeBackend{}
			epoch := uint32(1)
			refreshes := 0
			statuses := 0
			b.refresh = func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
				refreshes++
				return refreshData(epoch, []uint32{1}, fmt.Sprintf("epoch%d", epoch)), nil
			}
			b.search = func(context.Context, mail.SearchQuery) ([]uint32, error) {
				if !afterLookup && refreshes == 1 {
					epoch = 2
				}
				return []uint32{1}, nil
			}
			b.status = func(context.Context, string) (mail.MailboxStatus, error) {
				statuses++
				if afterLookup && statuses == 2 {
					epoch = 2
				}
				return mail.MailboxStatus{UIDValidity: epoch}, nil
			}
			s := newService(t, true, b)
			page, err := s.SearchMessages(context.Background(), mail.SearchQuery{Text: "live"})
			if err != nil || page.UIDValidity != 2 || page.Messages[0].Ref.UIDValidity != 2 || page.Messages[0].Subject != "epoch2 001" || refreshes != 2 {
				t.Fatalf("page=%+v err=%v refreshes=%d", page, err, refreshes)
			}
		})
	}
}

func TestLiveSearchRetriesMissingUIDOnce(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			b := &fakeBackend{}
			n := 0
			b.refresh = func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
				n++
				uids := []uint32{1}
				if recover && n == 2 {
					uids = []uint32{1, 2}
				}
				return refreshData(1, uids, "new"), nil
			}
			b.search = func(context.Context, mail.SearchQuery) ([]uint32, error) { return []uint32{2}, nil }
			s := newService(t, true, b)
			page, err := s.SearchMessages(context.Background(), mail.SearchQuery{Body: "live"})
			if recover {
				if err != nil || len(page.Messages) != 1 || page.Messages[0].Ref.UID != 2 {
					t.Fatalf("page=%+v err=%v", page, err)
				}
			} else {
				expectCode(t, err, "mailbox_changed")
				if len(page.Messages) != 0 {
					t.Fatal("partial results")
				}
			}
			if n != 2 || b.count("search") != 2 {
				t.Fatalf("refreshes=%d calls=%v", n, b.calls)
			}
		})
	}
}

func TestPersistentEpochChangesFailAfterOneRetry(t *testing.T) {
	b := &fakeBackend{}
	epoch := uint32(1)
	b.refresh = func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		return refreshData(epoch, []uint32{1}, "old"), nil
	}
	b.search = func(context.Context, mail.SearchQuery) ([]uint32, error) { epoch++; return []uint32{1}, nil }
	b.status = func(context.Context, string) (mail.MailboxStatus, error) {
		return mail.MailboxStatus{UIDValidity: epoch}, nil
	}
	s := newService(t, true, b)
	_, err := s.SearchMessages(context.Background(), mail.SearchQuery{Body: "live"})
	expectCode(t, err, "mailbox_changed")
	if b.count("refresh") != 2 {
		t.Fatalf("calls=%v", b.calls)
	}
}

func TestSortRoutingAndOrder(t *testing.T) {
	for _, ordering := range []string{"uid", "uid_desc", "uid_asc", "arrival", "arrival_desc", "date", "date_asc", "from", "subject", "subject_desc", "size", "size_desc", "to", "to_desc", "cc_asc"} {
		t.Run(ordering, func(t *testing.T) {
			b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
				return refreshData(1, []uint32{1, 2, 3}, "subject"), nil
			}, search: func(context.Context, mail.SearchQuery) ([]uint32, error) { return []uint32{2, 3, 1}, nil }}
			s := newService(t, true, b)
			page, err := s.ListMessages(context.Background(), mail.SearchQuery{Sort: ordering})
			if err != nil || page.Total != 3 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			live := strings.HasPrefix(ordering, "to") || strings.HasPrefix(ordering, "cc")
			if live {
				if b.count("search") != 1 || page.Messages[0].Ref.UID != 2 || page.Messages[1].Ref.UID != 3 {
					t.Fatalf("native order lost: %+v", page)
				}
			} else if b.count("search") != 0 {
				t.Fatalf("local sort went live: %v", b.calls)
			}
			if (ordering == "size" || ordering == "subject" || ordering == "uid_asc") && page.Messages[0].Ref.UID != 1 {
				t.Fatalf("ascending order lost: %+v", page)
			}
			if (ordering == "uid" || ordering == "uid_desc" || ordering == "size_desc" || ordering == "subject_desc") && page.Messages[0].Ref.UID != 3 {
				t.Fatalf("descending order lost: %+v", page)
			}
		})
	}
}

func TestAccountNamespace(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, true, b)
	rotated := New(testConfig(t, true, "mail.example.", "different-password", "user", "tls", "Trash", 993), b, s.store)
	if s.account != rotated.account {
		t.Fatal("password/normalized host changed namespace")
	}
	for _, cfg := range []*config.Config{testConfig(t, true, "other.example", "password", "user", "tls", "Trash", 993), testConfig(t, true, "mail.example", "password", "User", "tls", "Trash", 993), testConfig(t, true, "mail.example", "password", "user", "starttls", "Trash", 993), testConfig(t, true, "mail.example", "password", "user", "tls", "Trash", 994)} {
		if New(cfg, b, s.store).account == s.account {
			t.Fatal("distinct account collided")
		}
	}
	if s.key("inbox") != s.key("INBOX") || s.key("Archive") == s.key("archive") {
		t.Fatal("mailbox canonicalization exceeded INBOX")
	}
}

func TestMutationInvalidationIsDurableBeforeDispatch(t *testing.T) {
	operations := []struct {
		name     string
		affected []string
		run      func(*Service) (mail.MutationResult, error)
	}{
		{"add", []string{"INBOX"}, func(s *Service) (mail.MutationResult, error) {
			return s.AddFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{"\\Seen"}})
		}},
		{"remove", []string{"INBOX"}, func(s *Service) (mail.MutationResult, error) {
			return s.RemoveFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{"\\Deleted"}})
		}},
		{"copy", []string{"Archive"}, func(s *Service) (mail.MutationResult, error) {
			return s.CopyMessages(context.Background(), mail.TransferRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Destination: "Archive"})
		}},
		{"move", []string{"INBOX", "Archive"}, func(s *Service) (mail.MutationResult, error) {
			return s.MoveMessages(context.Background(), mail.TransferRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Destination: "Archive"})
		}},
		{"append", []string{"Archive"}, func(s *Service) (mail.MutationResult, error) {
			return s.AppendMessage(context.Background(), mail.AppendRequest{Mailbox: "Archive", MessageBase64: base64.StdEncoding.EncodeToString([]byte("Subject: test\r\n\r\nbody"))})
		}},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			b := &fakeBackend{}
			s := newService(t, false, b)
			for _, name := range []string{"INBOX", "Archive"} {
				if err := s.store.Apply(context.Background(), s.key(name), refreshData(1, []uint32{1}, "cached")); err != nil {
					t.Fatal(err)
				}
			}
			b.write = func(ctx context.Context, name string) (mail.MutationResult, error) {
				for _, box := range op.affected {
					state, err := s.store.State(ctx, s.key(box))
					if err != nil || state.HighestModSeq != 0 {
						t.Fatalf("pre-write state=%+v err=%v", state, err)
					}
				}
				return mail.MutationResult{}, &mail.Error{Code: "server_error", Message: "Mutation response was lost", OutcomeUnknown: true}
			}
			_, err := op.run(s)
			typed := expectCode(t, err, "server_error")
			if !typed.OutcomeUnknown || b.count(op.name) != 1 {
				t.Fatalf("write replayed or ambiguity lost: %v", b.calls)
			}
			if err := s.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := cache.Open(s.cfg.CachePath())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for _, box := range op.affected {
				state, err := reopened.State(context.Background(), s.key(box))
				if err != nil || state.HighestModSeq != 0 {
					t.Fatalf("durable state=%+v err=%v", state, err)
				}
			}
			if op.name == "copy" {
				state, err := reopened.State(context.Background(), s.key("INBOX"))
				if err != nil || state.HighestModSeq != 10 {
					t.Fatalf("copy invalidated source: %+v %v", state, err)
				}
			}
		})
	}
}

func TestReadonlyRejectsEveryMutationBeforeValidation(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, true, b)
	operations := []func() (mail.MutationResult, error){func() (mail.MutationResult, error) { return s.AddFlags(context.Background(), mail.FlagRequest{}) }, func() (mail.MutationResult, error) { return s.RemoveFlags(context.Background(), mail.FlagRequest{}) }, func() (mail.MutationResult, error) {
		return s.CopyMessages(context.Background(), mail.TransferRequest{})
	}, func() (mail.MutationResult, error) {
		return s.MoveMessages(context.Background(), mail.TransferRequest{})
	}, func() (mail.MutationResult, error) { return s.TrashMessages(context.Background(), nil) }, func() (mail.MutationResult, error) {
		return s.AppendMessage(context.Background(), mail.AppendRequest{})
	}}
	for _, run := range operations {
		_, err := run()
		expectCode(t, err, "readonly")
	}
	if len(b.calls) != 0 {
		t.Fatalf("backend calls=%v", b.calls)
	}
}

func TestInvalidationFailurePreventsAnyWriteDispatch(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, false, b)
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{"\\Seen"}})
	expectCode(t, err, "cache_error")
	if len(b.calls) != 0 || strings.Contains(err.Error(), "sql") {
		t.Fatalf("calls=%v error=%v", b.calls, err)
	}
	_, err = s.TrashMessages(context.Background(), []mail.MessageRef{ref("INBOX", 1)})
	expectCode(t, err, "cache_error")
	if len(b.calls) != 0 {
		t.Fatalf("Trash dispatched before invalidation: %v", b.calls)
	}
}

func TestMutationInputValidation(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, false, b)
	for _, refs := range [][]mail.MessageRef{nil, {ref("INBOX", 0)}, {{Mailbox: "INBOX", UID: 1}}, {ref("INBOX", 1), ref("inbox", 1)}, {ref("INBOX", 1), ref("Other", 2)}, {ref("INBOX", 1), {Mailbox: "INBOX", UID: 2, UIDValidity: 2}}, make([]mail.MessageRef, 201)} {
		_, err := s.AddFlags(context.Background(), mail.FlagRequest{Refs: refs, Flags: []string{"\\Seen"}})
		expectCode(t, err, "invalid_argument")
	}
	for _, flag := range []string{"\\Deleted", "\\Recent", "bad flag", "\\Other", ""} {
		_, err := s.AddFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{flag}})
		expectCode(t, err, "invalid_argument")
	}
	_, err := s.RemoveFlags(context.Background(), mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{"\\Recent"}})
	expectCode(t, err, "invalid_argument")
	_, err = s.MoveMessages(context.Background(), mail.TransferRequest{Refs: []mail.MessageRef{ref("inbox", 1)}, Destination: "INBOX"})
	expectCode(t, err, "invalid_argument")
	_, err = s.CopyMessages(context.Background(), mail.TransferRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}})
	expectCode(t, err, "invalid_argument")
	for _, q := range []mail.AppendRequest{{}, {MessageBase64: "bad base64"}, {MessageBase64: base64.StdEncoding.EncodeToString([]byte("message")), Flags: []string{"\\Deleted"}}} {
		_, err := s.AppendMessage(context.Background(), q)
		expectCode(t, err, "invalid_argument")
	}
	if len(b.calls) != 0 {
		t.Fatalf("backend calls=%v", b.calls)
	}
}

func TestTrashConfiguredCreationAndNativeMoveGate(t *testing.T) {
	cases := []struct {
		name   string
		info   mail.ServerInfo
		boxes  []mail.MailboxInfo
		source string
		code   string
		want   []string
	}{
		{"create", mail.ServerInfo{Capabilities: []string{"MOVE"}}, nil, "INBOX", "", []string{"info", "list", "create:Bin", "move"}},
		{"rev2", mail.ServerInfo{Capabilities: []string{"IMAP4rev2"}}, nil, "INBOX", "", []string{"info", "list", "create:Bin", "move"}},
		{"implemented", mail.ServerInfo{ImplementedFeatures: []string{"MOVE"}}, []mail.MailboxInfo{{Name: "Bin"}}, "INBOX", "", []string{"info", "list", "move"}},
		{"unsupported", mail.ServerInfo{}, nil, "INBOX", "unsupported", []string{"info"}},
		{"already", mail.ServerInfo{}, nil, "Bin", "invalid_argument", nil},
		{"noselect", mail.ServerInfo{Capabilities: []string{"MOVE"}}, []mail.MailboxInfo{{Name: "Bin", Attributes: []string{"\\Noselect"}}}, "INBOX", "invalid_argument", []string{"info", "list"}},
		{"exactname", mail.ServerInfo{Capabilities: []string{"MOVE"}}, []mail.MailboxInfo{{Name: "bin"}}, "INBOX", "", []string{"info", "list", "create:Bin", "move"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{info: tc.info, boxes: tc.boxes}
			s := newService(t, false, b)
			s.cfg = testConfig(t, false, "Mail.Example", "password", "user", "tls", "Bin", 993)
			for _, name := range []string{"INBOX", "Bin"} {
				if err := s.store.Apply(context.Background(), s.key(name), refreshData(1, []uint32{1}, "cached")); err != nil {
					t.Fatal(err)
				}
			}
			b.write = func(ctx context.Context, _ string) (mail.MutationResult, error) {
				for _, name := range []string{"INBOX", "Bin"} {
					state, err := s.store.State(ctx, s.key(name))
					if err != nil || state.HighestModSeq != 0 {
						t.Fatalf("not invalidated: %+v %v", state, err)
					}
				}
				return mail.MutationResult{}, nil
			}
			b.create = func(ctx context.Context, name string) error {
				if name != "Bin" {
					t.Fatalf("created unexpected mailbox %q", name)
				}
				for _, box := range []string{"INBOX", "Bin"} {
					state, err := s.store.State(ctx, s.key(box))
					if err != nil || state.HighestModSeq != 0 {
						t.Fatalf("creation preceded durable invalidation: %+v %v", state, err)
					}
				}
				return nil
			}
			_, err := s.TrashMessages(context.Background(), []mail.MessageRef{ref(tc.source, 1)})
			if tc.code != "" {
				expectCode(t, err, tc.code)
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(b.calls, tc.want) {
				t.Fatalf("calls=%v want=%v", b.calls, tc.want)
			}
		})
	}
}

func TestTrashCreationFailureDoesNotMoveOrReplay(t *testing.T) {
	b := &fakeBackend{info: mail.ServerInfo{Capabilities: []string{"MOVE"}}, createErr: errors.New("raw create response with password")}
	s := newService(t, false, b)
	_, err := s.TrashMessages(context.Background(), []mail.MessageRef{ref("INBOX", 1)})
	typed := expectCode(t, err, "server_error")
	if !typed.OutcomeUnknown || b.count("create:Trash") != 1 || b.count("move") != 0 || strings.Contains(err.Error(), "password") {
		t.Fatalf("calls=%v err=%v", b.calls, err)
	}
}

func TestContextWaitingForAccountLockCancels(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	b := &fakeBackend{refresh: func(context.Context, string, mail.CacheState) (mail.Refresh, error) {
		close(entered)
		<-release
		return refreshData(1, []uint32{1}, "locked"), nil
	}}
	s := newService(t, false, b)
	go func() { _, err := s.SearchMessages(context.Background(), mail.SearchQuery{}); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.AddFlags(ctx, mail.FlagRequest{Refs: []mail.MessageRef{ref("INBOX", 1)}, Flags: []string{"\\Seen"}})
	if !errors.Is(err, context.Canceled) || b.count("add") != 0 {
		t.Fatalf("error=%v calls=%v", err, b.calls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.SearchMessages(ctx, mail.SearchQuery{})
	if !errors.Is(err, context.DeadlineExceeded) || b.count("refresh") != 1 {
		t.Fatalf("error=%v calls=%v", err, b.calls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDirectReadsValidateCompleteReferencesAndDefaults(t *testing.T) {
	b := &fakeBackend{}
	s := newService(t, true, b)
	_, err := s.ReadMessage(context.Background(), mail.ReadRequest{})
	expectCode(t, err, "invalid_argument")
	_, err = s.ReadMessage(context.Background(), mail.ReadRequest{Ref: ref("INBOX", 1), MaxTextChars: mail.MaxTextChars + 1})
	expectCode(t, err, "invalid_argument")
	_, err = s.GetAttachment(context.Background(), mail.AttachmentRequest{Ref: ref("INBOX", 1)})
	expectCode(t, err, "invalid_argument")
	_, err = s.ThreadMessages(context.Background(), mail.ThreadRequest{Query: mail.SearchQuery{Mailbox: "Other"}})
	expectCode(t, err, "invalid_argument")
	if len(b.calls) != 0 {
		t.Fatalf("calls=%v", b.calls)
	}
	_, err = s.ReadMessage(context.Background(), mail.ReadRequest{Ref: ref("inbox", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if b.readRequest.Ref.Mailbox != "INBOX" || b.readRequest.MaxTextChars != mail.DefaultTextChars {
		t.Fatalf("read=%+v", b.readRequest)
	}
	_, err = s.ThreadMessages(context.Background(), mail.ThreadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if b.threadRequest.Mailbox != "INBOX" || b.threadRequest.Algorithm != "REFERENCES" || b.threadRequest.Query.Limit != 0 || b.threadRequest.Query.BeforeUID != 0 || b.threadRequest.Query.Sort != "" {
		t.Fatalf("thread=%+v", b.threadRequest)
	}
}
