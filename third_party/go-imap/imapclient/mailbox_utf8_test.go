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

type mailboxPeerStep struct {
	command  string
	response string
	body     string
}

func newMailboxPeer(t *testing.T, caps string, options *Options, steps []mailboxPeerStep) *Client {
	t.Helper()
	conn, peer := net.Pipe()
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(peer, "* PREAUTH [CAPABILITY %s] ready\r\n", caps); err != nil {
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
				done <- fmt.Errorf("wire command = %q, want %q", command, step.command)
				return
			}
			if step.body != "" {
				body := make([]byte, len(step.body)+2)
				if _, err := io.ReadFull(reader, body); err != nil {
					done <- err
					return
				}
				if string(body) != step.body+"\r\n" {
					done <- fmt.Errorf("APPEND body = %q", body)
					return
				}
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

func mailboxQuoted(name string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
}

func TestMailboxProtocolModes(t *testing.T) {
	modes := []struct {
		name, caps string
		enable     imap.Cap
		modern     bool
	}{
		{"rev1", "IMAP4rev1 MOVE LITERAL+", "", false},
		{"UTF8 advertised", "IMAP4rev1 ENABLE UTF8=ACCEPT MOVE LITERAL+", "", false},
		{"UTF8 enabled", "IMAP4rev1 ENABLE UTF8=ACCEPT MOVE LITERAL+", imap.CapUTF8Accept, true},
		{"UTF8 only enabled", "IMAP4rev1 ENABLE UTF8=ONLY MOVE LITERAL+", imap.CapUTF8Accept, true},
		{"dual advertised", "IMAP4rev1 IMAP4rev2 MOVE LITERAL+", "", false},
		{"dual enabled", "IMAP4rev1 IMAP4rev2 MOVE LITERAL+", imap.CapIMAP4rev2, true},
		{"pure rev2", "IMAP4rev2 MOVE LITERAL+", "", true},
		{"pure rev2 enabled", "IMAP4rev2 MOVE LITERAL+", imap.CapIMAP4rev2, true},
	}
	names := []struct{ raw, legacy string }{
		{"R&D", "R&-D"},
		{"R&-D", "R&--D"},
		{"&AOk-", "&-AOk-"},
		{"é & \"\\", "&AOk- &- \"\\"},
	}
	for _, mode := range modes {
		for _, name := range names {
			t.Run(mode.name+"/"+name.raw, func(t *testing.T) {
				wireName := name.legacy
				if mode.modern {
					wireName = name.raw
				}
				quoted := mailboxQuoted(wireName)
				var steps []mailboxPeerStep
				if mode.enable != "" {
					steps = append(steps, mailboxPeerStep{
						command:  "ENABLE " + string(mode.enable),
						response: "* ENABLED " + string(mode.enable) + "\r\n* LIST () NIL \"R&D\"\r\n{tag} OK enabled\r\n",
					})
				}
				steps = append(steps,
					mailboxPeerStep{command: `LIST "" ` + quoted, response: "* LIST () NIL " + quoted + "\r\n{tag} OK listed\r\n"},
					mailboxPeerStep{command: "STATUS " + quoted + " (MESSAGES)", response: fmt.Sprintf("* STATUS {%d}\r\n%s (MESSAGES 3)\r\n{tag} OK status\r\n", len(wireName), wireName)},
					mailboxPeerStep{command: "SELECT INBOX", response: "* 3 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n{tag} OK selected\r\n"},
					mailboxPeerStep{command: "UID COPY 1 " + quoted, response: "{tag} OK [COPYUID 7 1 2] copied\r\n"},
					mailboxPeerStep{command: "APPEND " + quoted + " {1+}", body: "x", response: "{tag} OK [APPENDUID 7 3] appended\r\n"},
					mailboxPeerStep{command: "UID MOVE 1 " + quoted, response: "{tag} OK [COPYUID 7 1 4] moved\r\n"},
				)
				listedDuringEnable := make(chan string, 1)
				options := &Options{UnilateralDataHandler: &UnilateralDataHandler{
					List: func(data *imap.ListData) { listedDuringEnable <- data.Mailbox },
				}}
				c := newMailboxPeer(t, mode.caps, options, steps)
				if mode.enable != "" {
					if _, err := c.Enable(mode.enable).Wait(); err != nil {
						t.Fatal(err)
					}
					if got := <-listedDuringEnable; got != "R&D" {
						t.Fatalf("LIST before ENABLE completion = %q", got)
					}
				}
				list, err := c.List("", name.raw, nil).Collect()
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].Mailbox != name.raw || list[0].Delim != 0 {
					t.Fatalf("LIST = %#v", list)
				}
				status, err := c.Status(name.raw, &imap.StatusOptions{NumMessages: true}).Wait()
				if err != nil {
					t.Fatal(err)
				}
				if status.Mailbox != name.raw || status.NumMessages == nil || *status.NumMessages != 3 {
					t.Fatalf("STATUS = %#v", status)
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Copy(imap.UIDSetNum(1), name.raw).Wait(); err != nil {
					t.Fatal(err)
				}
				appendCmd := c.Append(name.raw, 1, nil)
				if _, err := appendCmd.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				if err := appendCmd.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := appendCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Move(imap.UIDSetNum(1), name.raw).Wait(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMailboxEnableBeforeCompletion(t *testing.T) {
	for _, capability := range []imap.Cap{imap.CapUTF8Accept, imap.CapIMAP4rev2} {
		t.Run(string(capability), func(t *testing.T) {
			listed := make(chan string, 1)
			options := &Options{UnilateralDataHandler: &UnilateralDataHandler{
				List: func(data *imap.ListData) { listed <- data.Mailbox },
			}}
			c := newMailboxPeer(t, "IMAP4rev1 IMAP4rev2 ENABLE UTF8=ACCEPT", options, []mailboxPeerStep{
				{command: "ENABLE " + string(capability), response: "* ENABLED " + string(capability) + "\r\n* LIST () NIL \"R&D\"\r\n"},
				{command: `UID COPY 1 "R&D"`, response: "{tag} OK copied\r\nT1 OK enabled\r\n"},
			})
			enable := c.Enable(capability)
			select {
			case got := <-listed:
				if got != "R&D" {
					t.Fatalf("LIST before completion = %q", got)
				}
			case <-c.Closed():
				t.Fatal("client closed before decoding LIST after ENABLED")
			case <-time.After(5 * time.Second):
				t.Fatal("no LIST after ENABLED")
			}
			if _, err := c.Copy(imap.UIDSetNum(1), "R&D").Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := enable.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMailboxEnableUnconfirmed(t *testing.T) {
	for _, capability := range []imap.Cap{imap.CapUTF8Accept, imap.CapIMAP4rev2} {
		for _, response := range []string{"* ENABLED\r\n{tag} OK ignored\r\n", "{tag} BAD unsupported\r\n"} {
			t.Run(string(capability)+"/"+response, func(t *testing.T) {
				c := newMailboxPeer(t, "IMAP4rev1 IMAP4rev2 ENABLE UTF8=ACCEPT", nil, []mailboxPeerStep{
					{command: "ENABLE " + string(capability), response: response},
					{command: `LIST "" "R&-D"`, response: "* LIST () NIL \"R&-D\"\r\n{tag} OK listed\r\n"},
				})
				if _, err := c.Enable(capability).Wait(); (err != nil) != strings.Contains(response, "BAD") {
					t.Fatalf("ENABLE error = %v", err)
				}
				list, err := c.List("", "R&D", nil).Collect()
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].Mailbox != "R&D" {
					t.Fatalf("LIST = %#v", list)
				}
			})
		}
	}
}
