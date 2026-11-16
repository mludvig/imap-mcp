//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func TestMCPExactMailboxNames(t *testing.T) {
	binary := buildMCPBinary(t)
	for _, rev2 := range []bool{false, true} {
		for _, mode := range []string{"tls", "starttls"} {
			t.Run(fmt.Sprintf("rev2=%t/%s", rev2, mode), func(t *testing.T) {
				f := startDovecot(t, rev2)
				// doveadm bypasses IMAP encoding when creating the ambiguous names.
				for _, name := range []string{"R&D", "R&-D", "&AOk-", "é", "Trash&-Audit"} {
					docker(t, "exec", f.Container, "doveadm", "mailbox", "create", "-u", f.Username, name)
				}
				names := exactDoveadmMailboxes(t, f)
				configFile, _ := mcpFixtureConfig(t, f, mode, false, "Trash&Audit")
				p := startMCP(t, binary, configFile)
				info := mcpData[mail.ServerInfo](t, p, "server_info", map[string]any{})
				enabled := "UTF8=ACCEPT"
				if rev2 {
					enabled = "IMAP4rev2"
				}
				if !containsCapability(info.EnabledCapabilities, enabled) {
					t.Fatalf("%s not enabled: %+v", enabled, info)
				}
				assertExactMailboxList(t, p, names)
				for _, name := range []string{"R&D", "R&-D", "&AOk-", "é", "Trash&-Audit"} {
					status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{"mailbox": name})
					if status.Mailbox != name || status.Messages != 0 || status.UIDValidity == 0 {
						t.Fatalf("STATUS changed exact mailbox identity %q: %+v", name, status)
					}
				}
				expected := fixtureSnapshots(t, f, names)
				inbox := mcpData[mail.MessagePage](t, p, "list_messages", map[string]any{"limit": 200})
				refs := make(map[uint32]mail.MessageRef)
				for _, summary := range inbox.Messages {
					refs[summary.Ref.UID] = summary.Ref
				}

				transfer := func(tool, id, destination string) {
					t.Helper()
					source := refs[uint32(f.UIDs[id])]
					args := map[string]any{"refs": []mail.MessageRef{source}}
					if tool != "trash_messages" {
						args["destination"] = destination
					}
					result := mcpData[mail.MutationResult](t, p, tool, args)
					dest := exactDestinationRef(t, p, result, destination)
					if !reflect.DeepEqual(result.Affected, []mail.MessageRef{source}) {
						t.Fatalf("%s affected refs: %+v", tool, result)
					}
					message := expected["INBOX"][imap.UID(source.UID)]
					if tool != "copy_messages" {
						delete(expected["INBOX"], imap.UID(source.UID))
					}
					if expected[destination] == nil {
						expected[destination] = make(map[imap.UID]observedMessage)
						names = append(names, destination)
						sort.Strings(names)
					}
					message.UID = imap.UID(dest.UID)
					expected[destination][message.UID] = message
					assertAccountSnapshot(t, expected, fixtureSnapshots(t, f, names))
					assertExactDoveadmContents(t, f, "R&D", expected["R&D"])
					assertExactDoveadmContents(t, f, "R&-D", expected["R&-D"])
					assertExactDoveadmContents(t, f, destination, expected[destination])
					if tool != "copy_messages" {
						assertExactDoveadmContents(t, f, "INBOX", expected["INBOX"])
					}
					read := mcpData[mail.Message](t, p, "read_message", map[string]any{"ref": dest})
					if read.Summary.Ref != dest || !strings.Contains(read.Text, "Fixture body for "+id+".") {
						t.Fatalf("%s destination read has wrong identity: %+v", tool, read)
					}
				}
				transfer("copy_messages", "flagged", "R&D")

				raw := "From: append@example.test\r\nTo: fixture@example.test\r\n" +
					"Subject: Exact ampersand APPEND\r\nMessage-ID: <exact-append@fixture.test>\r\n\r\n" +
					"Append only to R&D.\r\n"
				date := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
				appended := mcpData[mail.MutationResult](t, p, "append_message", map[string]any{
					"mailbox": "R&D", "message_base64": base64.StdEncoding.EncodeToString([]byte(raw)),
					"flags": []string{`\Draft`, "ampersand-keyword"}, "internal_date": date.Format(time.RFC3339),
				})
				dest := exactDestinationRef(t, p, appended, "R&D")
				expected["R&D"][imap.UID(dest.UID)] = observedMessage{
					UID: imap.UID(dest.UID), Raw: raw, Flags: []imap.Flag{imap.FlagDraft, "ampersand-keyword"}, InternalDate: date,
				}
				assertAccountSnapshot(t, expected, fixtureSnapshots(t, f, names))
				assertExactDoveadmContents(t, f, "R&D", expected["R&D"])
				assertExactDoveadmContents(t, f, "R&-D", expected["R&-D"])

				transfer("move_messages", "page-02", "R&D")
				// The configured Trash must be created by the application, using
				// the exact ampersand name rather than its UTF-7-looking sibling.
				transfer("trash_messages", "page-03", "Trash&Audit")
				assertExactDoveadmContents(t, f, "Trash&-Audit", expected["Trash&-Audit"])
				if !reflect.DeepEqual(exactDoveadmMailboxes(t, f), names) {
					t.Fatalf("unexpected mailbox created: got %v, want %v", exactDoveadmMailboxes(t, f), names)
				}
				assertExactMailboxList(t, p, names)
				if !containsFlag(f.Snapshot(t, "INBOX")[f.UIDs["deleted"]].Flags, imap.FlagDeleted) {
					t.Fatal("unrelated Deleted sentinel was changed or removed")
				}
				assertAccountSnapshot(t, expected, fixtureSnapshots(t, f, names))
			})
		}
	}
}

func exactDestinationRef(t *testing.T, p *mcpProcess, result mail.MutationResult, mailbox string) mail.MessageRef {
	t.Helper()
	if !result.MappingAvailable || result.OutcomeUnknown || len(result.DestinationRefs) != 1 {
		t.Fatalf("missing authoritative UIDPLUS destination identity: %+v", result)
	}
	ref := result.DestinationRefs[0]
	status := mcpData[mail.MailboxStatus](t, p, "mailbox_status", map[string]any{"mailbox": mailbox})
	if ref.Mailbox != mailbox || ref.UID == 0 || ref.UIDValidity == 0 || ref.UIDValidity != status.UIDValidity {
		t.Fatalf("destination reference disagrees with exact STATUS: %+v / %+v", ref, status)
	}
	return ref
}

func exactDoveadmMailboxes(t *testing.T, f *dovecotFixture) []string {
	t.Helper()
	names := strings.Split(docker(t, "exec", f.Container, "doveadm", "mailbox", "list", "-u", f.Username), "\n")
	sort.Strings(names)
	return names
}

func assertExactMailboxList(t *testing.T, p *mcpProcess, want []string) {
	t.Helper()
	list := mcpData[[]mail.MailboxInfo](t, p, "list_mailboxes", map[string]any{})
	got := make([]string, 0, len(list))
	for _, box := range list {
		got = append(got, box.Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LIST changed exact mailbox names: got %v, want %v", got, want)
	}
}

func assertExactDoveadmContents(t *testing.T, f *dovecotFixture, mailbox string, want map[imap.UID]observedMessage) {
	t.Helper()
	status := docker(t, "exec", f.Container, "doveadm", "mailbox", "status", "-u", f.Username, "messages", mailbox)
	fields := strings.Fields(status)
	if len(fields) != 2 || fields[0] != mailbox || fields[1] != fmt.Sprintf("messages=%d", len(want)) {
		t.Fatalf("independent exact mailbox count for %q: %q, want %d", mailbox, status, len(want))
	}
	if len(want) == 0 {
		return
	}
	output := docker(t, "exec", f.Container, "doveadm", "-f", "json", "fetch", "-u", f.Username, "uid text", "mailbox", mailbox)
	var messages []struct {
		UID  string `json:"uid"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(output), &messages); err != nil {
		t.Fatalf("decode independent raw messages for %q: %v: %s", mailbox, err, output)
	}
	if len(messages) != len(want) {
		t.Fatalf("independent raw message count for %q: got %d, want %d", mailbox, len(messages), len(want))
	}
	seen := make(map[imap.UID]bool)
	for _, message := range messages {
		uid, err := strconv.ParseUint(message.UID, 10, 32)
		expected, exists := want[imap.UID(uid)]
		// Maildir text uses LF; IMAP reconstructs CRLF on FETCH.
		hash := func(raw string) [32]byte { return sha256.Sum256([]byte(strings.ReplaceAll(raw, "\r\n", "\n"))) }
		if err != nil || !exists || seen[imap.UID(uid)] || hash(message.Text) != hash(expected.Raw) {
			t.Fatalf("independent raw identity in exact mailbox %q: UID=%q exists=%t raw hash=%x, want=%x (%v)",
				mailbox, message.UID, exists, hash(message.Text), hash(expected.Raw), err)
		}
		seen[imap.UID(uid)] = true
	}
}
