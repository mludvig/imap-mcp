package server

import (
	"time"

	"github.com/mludvig/imap-mcp/internal/mail"
)

// Optional DTO fields keep the SDK schemas useful for minimal tool calls.
type queryInput struct {
	Mailbox    string            `json:"mailbox,omitempty" jsonschema:"Mailbox; defaults to INBOX."`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	Cc         string            `json:"cc,omitempty"`
	Bcc        string            `json:"bcc,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Body       string            `json:"body,omitempty"`
	Text       string            `json:"text,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Since      string            `json:"since,omitempty" jsonschema:"Inclusive internal civil date YYYY-MM-DD."`
	Before     string            `json:"before,omitempty" jsonschema:"Exclusive internal civil date YYYY-MM-DD."`
	SentSince  string            `json:"sent_since,omitempty" jsonschema:"Inclusive sent civil date YYYY-MM-DD."`
	SentBefore string            `json:"sent_before,omitempty" jsonschema:"Exclusive sent civil date YYYY-MM-DD."`
	Flags      []string          `json:"flags,omitempty"`
	NotFlags   []string          `json:"not_flags,omitempty"`
	MinSize    int64             `json:"min_size,omitempty"`
	MaxSize    int64             `json:"max_size,omitempty"`
	Limit      int               `json:"limit,omitempty" jsonschema:"Returned page size; defaults to 50, maximum 200. Does not bound header synchronization."`
	BeforeUID  uint32            `json:"before_uid,omitempty" jsonschema:"Exclusive UID cursor; only with default UID descending sort."`
	Sort       string            `json:"sort,omitempty" jsonschema:"Default UID descending; other sorts require server support."`
}

func (in queryInput) query() mail.SearchQuery {
	limit := in.Limit
	if limit == 0 {
		limit = mail.DefaultLimit
	}
	return mail.SearchQuery{
		Mailbox: mailbox(in.Mailbox), From: in.From, To: in.To, Cc: in.Cc,
		Bcc: in.Bcc, Subject: in.Subject, Body: in.Body, Text: in.Text,
		Headers: in.Headers, Since: in.Since, Before: in.Before,
		SentSince: in.SentSince, SentBefore: in.SentBefore, Flags: in.Flags,
		NotFlags: in.NotFlags, MinSize: in.MinSize, MaxSize: in.MaxSize,
		Limit: limit, BeforeUID: in.BeforeUID, Sort: in.Sort,
	}
}

type mailboxInput struct {
	Mailbox string `json:"mailbox,omitempty" jsonschema:"Mailbox; defaults to INBOX."`
}

type listInput struct {
	Mailbox   string `json:"mailbox,omitempty" jsonschema:"Mailbox; defaults to INBOX."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Returned page size; defaults to 50, maximum 200. Does not bound header synchronization."`
	BeforeUID uint32 `json:"before_uid,omitempty" jsonschema:"Exclusive UID cursor; only with default UID descending sort."`
	Sort      string `json:"sort,omitempty" jsonschema:"Default UID descending; other sorts require server support."`
}

type readInput struct {
	Ref          mail.MessageRef `json:"ref"`
	IncludeHTML  bool            `json:"include_html,omitempty"`
	MaxTextChars int             `json:"max_text_chars,omitempty" jsonschema:"Defaults to 20000 characters; maximum 200000."`
}

// THREAD returns complete trees; search pagination and sorting do not apply.
// See docs/concepts/stdio-mcp.md for the separate THREAD criteria contract.
type threadCriteriaInput struct {
	Mailbox    string            `json:"mailbox,omitempty" jsonschema:"Mailbox; defaults to the enclosing mailbox."`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	Cc         string            `json:"cc,omitempty"`
	Bcc        string            `json:"bcc,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Body       string            `json:"body,omitempty"`
	Text       string            `json:"text,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Since      string            `json:"since,omitempty" jsonschema:"Inclusive internal civil date YYYY-MM-DD."`
	Before     string            `json:"before,omitempty" jsonschema:"Exclusive internal civil date YYYY-MM-DD."`
	SentSince  string            `json:"sent_since,omitempty" jsonschema:"Inclusive sent civil date YYYY-MM-DD."`
	SentBefore string            `json:"sent_before,omitempty" jsonschema:"Exclusive sent civil date YYYY-MM-DD."`
	Flags      []string          `json:"flags,omitempty"`
	NotFlags   []string          `json:"not_flags,omitempty"`
	MinSize    int64             `json:"min_size,omitempty"`
	MaxSize    int64             `json:"max_size,omitempty"`
}

func (in threadCriteriaInput) query() mail.SearchQuery {
	return mail.SearchQuery{
		Mailbox: in.Mailbox, From: in.From, To: in.To, Cc: in.Cc,
		Bcc: in.Bcc, Subject: in.Subject, Body: in.Body, Text: in.Text,
		Headers: in.Headers, Since: in.Since, Before: in.Before,
		SentSince: in.SentSince, SentBefore: in.SentBefore, Flags: in.Flags,
		NotFlags: in.NotFlags, MinSize: in.MinSize, MaxSize: in.MaxSize,
	}
}

type threadInput struct {
	Mailbox   string              `json:"mailbox,omitempty" jsonschema:"Mailbox; defaults to INBOX."`
	Algorithm string              `json:"algorithm,omitempty" jsonschema:"Server-supported THREAD algorithm; defaults to REFERENCES."`
	Query     threadCriteriaInput `json:"query,omitempty" jsonschema:"Optional criteria combined with AND; mailbox defaults to the enclosing mailbox."`
}

type trashInput struct {
	Refs []mail.MessageRef `json:"refs"`
}

type appendInput struct {
	Mailbox       string     `json:"mailbox,omitempty" jsonschema:"Destination mailbox; defaults to INBOX."`
	MessageBase64 string     `json:"message_base64" jsonschema:"Base64 RFC 5322 message, at most 10 MiB decoded."`
	Flags         []string   `json:"flags,omitempty"`
	InternalDate  *time.Time `json:"internal_date,omitempty" jsonschema:"Optional RFC 3339 internal date."`
}

func mailbox(name string) string {
	if name == "" {
		return "INBOX"
	}
	return name
}
