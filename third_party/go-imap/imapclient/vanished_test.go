package imapclient

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

type vanishedPeerStep struct {
	command  string
	response string
}

func newVanishedPeer(t *testing.T, options *Options, steps ...vanishedPeerStep) *Client {
	t.Helper()
	conn, peer := net.Pipe()
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(peer, "* PREAUTH [CAPABILITY IMAP4rev1 IMAP4rev2 ENABLE MOVE] ready\r\n"); err != nil {
			done <- err
			return
		}
		reader := bufio.NewReader(peer)
		for _, step := range steps {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
			if command != step.command {
				done <- fmt.Errorf("command = %q, want %q", command, step.command)
				return
			}
			if _, err := io.WriteString(peer, strings.ReplaceAll(step.response, "{tag}", tag)); err != nil {
				done <- err
				return
			}
		}
		<-finish
		done <- nil
	}()
	c := New(conn, options)
	t.Cleanup(func() {
		close(finish)
		c.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	return c
}

func vanishedSelectSteps(num uint32) []vanishedPeerStep {
	return []vanishedPeerStep{
		{"ENABLE IMAP4rev2", "* ENABLED IMAP4rev2\r\n{tag} OK enabled\r\n"},
		{"SELECT INBOX", fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n{tag} OK [READ-WRITE] selected\r\n", num)},
		{"NOOP", "{tag} OK ready\r\n"},
	}
}

func selectVanishedMailbox(t *testing.T, c *Client) {
	t.Helper()
	if _, err := c.Enable(imap.CapIMAP4rev2).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// Order the check after SELECT's state update, which follows completion.
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestVanishedMove(t *testing.T) {
	steps := append(vanishedSelectSteps(5),
		vanishedPeerStep{"UID MOVE 10:11 \"Archive\"", "* OK [COPYUID 9 10:11 20:21] copied\r\n* VANISHED 10:11\r\n{tag} OK moved\r\n"},
		vanishedPeerStep{"NOOP", "* 4 EXISTS\r\n* VANISHED 12\r\n* FLAGS (\\Seen)\r\n{tag} OK updated\r\n"},
		vanishedPeerStep{"NOOP", "{tag} OK still usable\r\n"},
	)
	c := newVanishedPeer(t, nil, steps...)
	selectVanishedMailbox(t, c)
	data, err := c.Move(imap.UIDSet{{Start: 10, Stop: 11}}, "Archive").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if data.UIDValidity != 9 || data.SourceUIDs.String() != "10:11" || data.DestUIDs.String() != "20:21" {
		t.Fatalf("MOVE mapping = %#v", data)
	}
	if got := c.Mailbox().NumMessages; got != 3 {
		t.Fatalf("count after MOVE = %d, want 3", got)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if got := c.Mailbox().NumMessages; got != 3 {
		t.Fatalf("count after interleaved responses = %d, want 3", got)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestVanishedCountsAndCallback(t *testing.T) {
	tests := []struct {
		name, response, uids string
		initial, want        uint32
		earlier              bool
	}{
		{"multiple ranges", " 1:3,7:8", "1:3,7:8", 8, 3, false},
		{"overlapping ranges", " 1:3,2:5,5", "1:5", 8, 3, false},
		{"reversed range", " 3:1", "1:3", 8, 5, false},
		{"maximum UID", " 4294967295", "4294967295", 8, 7, false},
		{"range ending at maximum", " 4294967293:4294967295", "4294967293:4294967295", 8, 5, false},
		{"full UID space", " 1:4294967295", "1:4294967295", ^uint32(0), 0, false},
		{"count cannot underflow", " 1:4294967295", "1:4294967295", 8, 0, false},
		{"empty mailbox", " 1", "1", 0, 0, false},
		{"earlier", " (EARLIER) 1:4294967295", "1:4294967295", 8, 8, true},
		{"case insensitive earlier", " (earlier) 2:3", "2:3", 8, 8, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type notification struct {
				uids    string
				earlier bool
				count   uint32
			}
			notified := make(chan notification, 1)
			var c *Client
			options := &Options{UnilateralDataHandler: &UnilateralDataHandler{
				Vanished: func(data *UnilateralDataVanished) {
					// Both getters take the mutex: this also checks callback lock safety.
					c.State()
					notified <- notification{data.UIDs.String(), data.Earlier, c.Mailbox().NumMessages}
				},
				Expunge: func(uint32) { t.Error("VANISHED must not synthesize EXPUNGE") },
			}}
			steps := append(vanishedSelectSteps(tt.initial),
				vanishedPeerStep{"NOOP", "* VANISHED" + tt.response + "\r\n{tag} OK removed\r\n"},
				vanishedPeerStep{"NOOP", "{tag} OK still usable\r\n"},
			)
			c = newVanishedPeer(t, options, steps...)
			selectVanishedMailbox(t, c)
			before := c.Mailbox()
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			got := <-notified
			if got.uids != tt.uids || got.earlier != tt.earlier || got.count != tt.want {
				t.Fatalf("notification = %+v, want UIDs %q, Earlier %v, count %d", got, tt.uids, tt.earlier, tt.want)
			}
			if count := c.Mailbox().NumMessages; count != tt.want {
				t.Fatalf("count = %d, want %d", count, tt.want)
			}
			if before.NumMessages != tt.initial {
				t.Fatal("previous mailbox snapshot changed")
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVanishedMalformed(t *testing.T) {
	responses := []string{
		"* VANISHED\r\n",
		"* VANISHED 0\r\n",
		"* VANISHED *\r\n",
		"* VANISHED 1:*\r\n",
		"* VANISHED $\r\n",
		"* VANISHED 4294967296\r\n",
		"* VANISHED 1:\r\n",
		"* VANISHED 1,,2\r\n",
		"* VANISHED (UNKNOWN) 1\r\n",
		"* VANISHED (EARLIER EXTRA) 1\r\n",
		"* VANISHED (EARLIER)1\r\n",
		"* VANISHED(EARLIER) 1\r\n",
		"* VANISHED 1 extra\r\n",
	}
	for _, response := range responses {
		t.Run(strings.TrimSpace(response), func(t *testing.T) {
			steps := append(vanishedSelectSteps(5), vanishedPeerStep{"NOOP", response})
			c := newVanishedPeer(t, nil, steps...)
			selectVanishedMailbox(t, c)
			if err := c.Noop().Wait(); err == nil {
				t.Fatal("malformed VANISHED succeeded")
			}
			select {
			case <-c.Closed():
			case <-time.After(3 * time.Second):
				t.Fatal("decoder did not close after malformed VANISHED")
			}
		})
	}
}
