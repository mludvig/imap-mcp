// Package mail defines the shared, protocol-independent mail contracts.
package mail

import (
	"context"
	"time"
)

const (
	MaxMessageBytes    = 10 * 1024 * 1024
	MaxAttachmentBytes = 2 * 1024 * 1024
	MaxHeaderBytes     = 256 * 1024
	DefaultLimit       = 50
	MaxLimit           = 200
	DefaultTextChars   = 20000
	MaxTextChars       = 200000
)

// MessageRef identifies a message within one mailbox UIDVALIDITY epoch.
type MessageRef struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	UID         uint32 `json:"uid"`
}

type MailboxKey struct {
	Account string `json:"account"`
	Name    string `json:"name"`
}

type CacheState struct {
	Present       bool     `json:"present"`
	UIDValidity   uint32   `json:"uid_validity"`
	HighestModSeq uint64   `json:"highest_mod_seq"`
	CachedUIDs    []uint32 `json:"cached_uids"`
}

// Metadata contains only the raw header and message metadata, never the body.
type Metadata struct {
	UID          uint32    `json:"uid"`
	InternalDate time.Time `json:"internal_date"`
	Size         int64     `json:"size"`
	Header       []byte    `json:"header"`
}

type FlagUpdate struct {
	UID    uint32   `json:"uid"`
	Flags  []string `json:"flags"`
	ModSeq uint64   `json:"mod_seq"`
}

// Refresh carries complete membership and incremental metadata and flag changes.
type Refresh struct {
	UIDValidity   uint32       `json:"uid_validity"`
	HighestModSeq uint64       `json:"highest_mod_seq"`
	Members       []uint32     `json:"members"`
	NewMetadata   []Metadata   `json:"new_metadata"`
	FlagUpdates   []FlagUpdate `json:"flag_updates"`
}

// MessageSummary contains a decoded subject and decoded, formatted address strings.
// A zero ModSeq means the server did not provide a modification sequence.
type MessageSummary struct {
	Ref          MessageRef `json:"ref"`
	Subject      string     `json:"subject"`
	From         []string   `json:"from"`
	To           []string   `json:"to"`
	Cc           []string   `json:"cc"`
	Bcc          []string   `json:"bcc"`
	Date         *time.Time `json:"date,omitempty"`
	InternalDate time.Time  `json:"internal_date"`
	Size         int64      `json:"size"`
	Flags        []string   `json:"flags"`
	ModSeq       uint64     `json:"mod_seq,omitempty"`
}

// SearchQuery uses ISO civil dates (YYYY-MM-DD) for its date constraints.
// Empty Sort defaults to UID order, newest first. BeforeUID is an exclusive cursor.
type SearchQuery struct {
	Mailbox    string            `json:"mailbox"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	Cc         string            `json:"cc"`
	Bcc        string            `json:"bcc"`
	Subject    string            `json:"subject"`
	Body       string            `json:"body"`
	Text       string            `json:"text"`
	Headers    map[string]string `json:"headers"`
	Since      string            `json:"since"`
	Before     string            `json:"before"`
	SentSince  string            `json:"sent_since"`
	SentBefore string            `json:"sent_before"`
	Flags      []string          `json:"flags"`
	NotFlags   []string          `json:"not_flags"`
	MinSize    int64             `json:"min_size"`
	MaxSize    int64             `json:"max_size"`
	Limit      int               `json:"limit"`
	BeforeUID  uint32            `json:"before_uid"`
	Sort       string            `json:"sort"`
}

type MessagePage struct {
	Messages      []MessageSummary `json:"messages"`
	NextBeforeUID uint32           `json:"next_before_uid,omitempty"`
	UIDValidity   uint32           `json:"uid_validity"`
	Total         int              `json:"total"`
}

type ServerInfo struct {
	Capabilities        []string `json:"capabilities"`
	EnabledCapabilities []string `json:"enabled_capabilities"`
	ImplementedFeatures []string `json:"implemented_features"`
	Readonly            bool     `json:"readonly"`
}

type MailboxInfo struct {
	Name       string   `json:"name"`
	Delimiter  string   `json:"delimiter"`
	Attributes []string `json:"attributes"`
}

type MailboxStatus struct {
	Mailbox       string `json:"mailbox"`
	Messages      uint32 `json:"messages"`
	Unseen        uint32 `json:"unseen"`
	UIDNext       uint32 `json:"uid_next"`
	UIDValidity   uint32 `json:"uid_validity"`
	HighestModSeq uint64 `json:"highest_mod_seq"`
}

type ReadRequest struct {
	Ref          MessageRef `json:"ref"`
	IncludeHTML  bool       `json:"include_html"`
	MaxTextChars int        `json:"max_text_chars"`
}

type Message struct {
	Summary     MessageSummary   `json:"summary"`
	Text        string           `json:"text"`
	HTML        string           `json:"html"`
	Truncated   bool             `json:"truncated"`
	Attachments []AttachmentInfo `json:"attachments"`
}

type AttachmentInfo struct {
	PartID      string `json:"part_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Disposition string `json:"disposition"`
	Size        int64  `json:"size"`
}

type AttachmentRequest struct {
	Ref    MessageRef `json:"ref"`
	PartID string     `json:"part_id"`
}

type Attachment struct {
	Info       AttachmentInfo `json:"info"`
	DataBase64 string         `json:"data_base64"`
}

type FlagRequest struct {
	Refs  []MessageRef `json:"refs"`
	Flags []string     `json:"flags"`
}

type TransferRequest struct {
	Refs        []MessageRef `json:"refs"`
	Destination string       `json:"destination"`
}

type MutationResult struct {
	Affected         []MessageRef `json:"affected"`
	DestinationRefs  []MessageRef `json:"destination_refs"`
	MappingAvailable bool         `json:"mapping_available"`
	OutcomeUnknown   bool         `json:"outcome_unknown"`
}

type AppendRequest struct {
	Mailbox       string     `json:"mailbox"`
	MessageBase64 string     `json:"message_base64"`
	Flags         []string   `json:"flags"`
	InternalDate  *time.Time `json:"internal_date,omitempty"`
}

type ThreadRequest struct {
	Mailbox   string      `json:"mailbox"`
	Algorithm string      `json:"algorithm"`
	Query     SearchQuery `json:"query"`
}

type ThreadNode struct {
	Ref      MessageRef   `json:"ref"`
	Children []ThreadNode `json:"children"`
}

type ThreadResult struct {
	Threads []ThreadNode `json:"threads"`
}

type Error struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	OutcomeUnknown bool   `json:"outcome_unknown"`
}

func (e *Error) Error() string {
	return e.Message
}

func NewError(code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

type Backend interface {
	Info(context.Context) (ServerInfo, error)
	ListMailboxes(context.Context) ([]MailboxInfo, error)
	MailboxStatus(context.Context, string) (MailboxStatus, error)
	Refresh(context.Context, string, CacheState) (Refresh, error)
	SearchUIDs(context.Context, SearchQuery) ([]uint32, error)
	ReadMessage(context.Context, ReadRequest) (Message, error)
	GetAttachment(context.Context, AttachmentRequest) (Attachment, error)
	ThreadMessages(context.Context, ThreadRequest) (ThreadResult, error)
	AddFlags(context.Context, FlagRequest) (MutationResult, error)
	RemoveFlags(context.Context, FlagRequest) (MutationResult, error)
	CopyMessages(context.Context, TransferRequest) (MutationResult, error)
	MoveMessages(context.Context, TransferRequest) (MutationResult, error)
	AppendMessage(context.Context, AppendRequest) (MutationResult, error)
	CreateMailbox(context.Context, string) error
}
