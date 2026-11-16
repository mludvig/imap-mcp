package client

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/mludvig/imap-mcp/internal/mail"
)

func parseFlags(input []string, adding bool) ([]imap.Flag, error) {
	result := make([]imap.Flag, 0, len(input))
	for _, flag := range input {
		if flag == "" {
			return nil, mail.NewError("invalid_argument", "Flag must not be empty")
		}
		atom := flag
		if strings.HasPrefix(atom, "\\") {
			switch strings.ToLower(atom) {
			case "\\seen":
				flag = string(imap.FlagSeen)
			case "\\answered":
				flag = string(imap.FlagAnswered)
			case "\\flagged":
				flag = string(imap.FlagFlagged)
			case "\\deleted":
				flag = string(imap.FlagDeleted)
			case "\\draft":
				flag = string(imap.FlagDraft)
			case "\\recent":
				flag = "\\Recent"
			default:
				return nil, mail.NewError("invalid_argument", "Unknown system flag")
			}
			atom = atom[1:]
		}
		for _, ch := range []byte(atom) {
			if ch < 0x21 || ch > 0x7e || strings.ContainsRune("(){%*\"\\]}", rune(ch)) {
				return nil, mail.NewError("invalid_argument", "Flag must be an IMAP atom")
			}
		}
		if adding && strings.EqualFold(flag, string(imap.FlagDeleted)) {
			return nil, mail.NewError("invalid_argument", "Adding the Deleted flag is forbidden")
		}
		result = append(result, imap.Flag(flag))
	}
	return result, nil
}
func mutationFlags(input []string, adding bool) ([]imap.Flag, error) {
	flags, err := parseFlags(input, adding)
	if err != nil {
		return nil, err
	}
	for _, flag := range flags {
		if flag == imap.Flag("\\Recent") {
			return nil, mail.NewError("invalid_argument", "The Recent flag cannot be changed")
		}
	}
	return flags, nil
}
func (c *Client) writeGuard() error {
	if c.cfg.Readonly() {
		return mail.NewError("readonly", "This server is configured read-only; restart with readonly=false to enable writes")
	}
	return nil
}
func validateRefs(refs []mail.MessageRef) (imap.UIDSet, error) {
	if len(refs) == 0 {
		return nil, mail.NewError("invalid_argument", "At least one message reference is required")
	}
	seen := map[uint32]bool{}
	set := imap.UIDSet{}
	for _, ref := range refs {
		if err := checkRef(ref, refs[0].UIDValidity); err != nil {
			return nil, err
		}
		if ref.Mailbox != refs[0].Mailbox {
			return nil, mail.NewError("invalid_argument", "Message references must share one mailbox and UIDVALIDITY")
		}
		if seen[ref.UID] {
			return nil, mail.NewError("invalid_argument", "Duplicate message reference")
		}
		seen[ref.UID] = true
		set.AddNum(imap.UID(ref.UID))
	}
	return set, nil
}
func (s *session) selectForWrite(refs []mail.MessageRef, set imap.UIDSet) error {
	selected, err := s.selectMailbox(refs[0].Mailbox, false)
	if err != nil {
		return err
	}
	if err := checkRef(refs[0], selected.UIDValidity); err != nil {
		return err
	}
	if selected.ReadOnly {
		return mail.NewError("readonly", "The IMAP mailbox is read-only")
	}
	data, err := s.fetch(set, &imap.FetchOptions{UID: true}, nil, mail.MaxHeaderBytes)
	if err != nil {
		return err
	}
	expected := map[uint32]bool{}
	for _, ref := range refs {
		expected[ref.UID] = true
	}
	for _, item := range data {
		if !expected[item.uid] {
			return mail.NewError("server_error", "IMAP server returned an unexpected UID")
		}
		delete(expected, item.uid)
	}
	if len(expected) != 0 {
		return mail.NewError("not_found", "One or more messages no longer exist")
	}
	return nil
}
func (c *Client) store(ctx context.Context, q mail.FlagRequest, adding bool) (mail.MutationResult, error) {
	if err := c.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	set, err := validateRefs(q.Refs)
	if err != nil {
		return mail.MutationResult{}, err
	}
	if len(q.Flags) == 0 {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "At least one flag is required")
	}
	flags, err := mutationFlags(q.Flags, adding)
	if err != nil {
		return mail.MutationResult{}, err
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.MutationResult{}, err
	}
	defer s.close()
	if err := s.selectForWrite(q.Refs, set); err != nil {
		return mail.MutationResult{}, err
	}
	op := imap.StoreFlagsDel
	if adding {
		op = imap.StoreFlagsAdd
	}
	err = s.c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil).Close()
	if err != nil {
		return mail.MutationResult{}, s.err(err, true)
	}
	return mail.MutationResult{Affected: q.Refs, DestinationRefs: []mail.MessageRef{}}, nil
}
func (c *Client) AddFlags(ctx context.Context, q mail.FlagRequest) (mail.MutationResult, error) {
	return c.store(ctx, q, true)
}
func (c *Client) RemoveFlags(ctx context.Context, q mail.FlagRequest) (mail.MutationResult, error) {
	return c.store(ctx, q, false)
}

func copyMapping(refs []mail.MessageRef, destination string, epoch uint32, source, dest imap.NumSet) (mail.MutationResult, error) {
	result := mail.MutationResult{Affected: refs, DestinationRefs: []mail.MessageRef{}}
	if epoch == 0 || source == nil || dest == nil {
		return result, nil
	}
	src, ok := source.(imap.UIDSet)
	if !ok {
		return mail.MutationResult{}, mail.NewError("server_error", "IMAP transfer mapping used sequence numbers")
	}
	dst, ok := dest.(imap.UIDSet)
	if !ok {
		return mail.MutationResult{}, mail.NewError("server_error", "IMAP transfer mapping used sequence numbers")
	}
	sourceUIDs, err := expandUIDs(src, uint64(len(refs)))
	if err != nil {
		return mail.MutationResult{}, err
	}
	destUIDs, err := expandUIDs(dst, uint64(len(refs)))
	if err != nil {
		return mail.MutationResult{}, err
	}
	if len(sourceUIDs) != len(destUIDs) || len(sourceUIDs) == 0 {
		return mail.MutationResult{}, mail.NewError("server_error", "IMAP transfer mapping is incomplete")
	}
	expected := map[uint32]mail.MessageRef{}
	for _, ref := range refs {
		expected[ref.UID] = ref
	}
	result.Affected = []mail.MessageRef{}
	for i, uid := range sourceUIDs {
		ref, ok := expected[uid]
		if !ok {
			return mail.MutationResult{}, mail.NewError("server_error", "IMAP transfer mapping contains an unexpected source UID")
		}
		delete(expected, uid)
		result.Affected = append(result.Affected, ref)
		result.DestinationRefs = append(result.DestinationRefs, mail.MessageRef{Mailbox: destination, UIDValidity: epoch, UID: destUIDs[i]})
	}
	result.MappingAvailable = true
	return result, nil
}
func (c *Client) transfer(ctx context.Context, q mail.TransferRequest, move bool) (mail.MutationResult, error) {
	if err := c.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	set, err := validateRefs(q.Refs)
	if err != nil {
		return mail.MutationResult{}, err
	}
	if q.Destination == "" {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "Destination mailbox is required")
	}
	if move && q.Destination == q.Refs[0].Mailbox {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "Move destination must differ from the source mailbox")
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.MutationResult{}, err
	}
	defer s.close()
	// This gate MUST precede Move: beta.9 otherwise sends Deleted + EXPUNGE.
	// See docs/concepts/imap-safety.md.
	if move && !s.caps.Has(imap.CapMove) {
		return mail.MutationResult{}, mail.NewError("unsupported", "The IMAP server does not support native MOVE")
	}
	if err := s.selectForWrite(q.Refs, set); err != nil {
		return mail.MutationResult{}, err
	}
	var result mail.MutationResult
	if move && !s.c.Caps().Has(imap.CapMove) {
		return mail.MutationResult{}, mail.NewError("unsupported", "The IMAP server does not support native MOVE")
	}
	if move {
		data, err := s.c.Move(set, q.Destination).Wait()
		if err != nil {
			return mail.MutationResult{}, s.err(err, true)
		}
		result, err = copyMapping(q.Refs, q.Destination, data.UIDValidity, data.SourceUIDs, data.DestUIDs)
	} else {
		data, copyErr := s.c.Copy(set, q.Destination).Wait()
		if copyErr != nil {
			return mail.MutationResult{}, s.err(copyErr, true)
		}
		result, err = copyMapping(q.Refs, q.Destination, data.UIDValidity, data.SourceUIDs, data.DestUIDs)
	}
	if err != nil {
		if e, ok := err.(*mail.Error); ok {
			e.OutcomeUnknown = true
		}
		return mail.MutationResult{}, err
	}
	return result, nil
}
func (c *Client) CopyMessages(ctx context.Context, q mail.TransferRequest) (mail.MutationResult, error) {
	return c.transfer(ctx, q, false)
}
func (c *Client) MoveMessages(ctx context.Context, q mail.TransferRequest) (mail.MutationResult, error) {
	return c.transfer(ctx, q, true)
}
func (c *Client) AppendMessage(ctx context.Context, q mail.AppendRequest) (mail.MutationResult, error) {
	if err := c.writeGuard(); err != nil {
		return mail.MutationResult{}, err
	}
	if q.Mailbox == "" {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "Mailbox is required")
	}
	if len(q.MessageBase64) > base64.StdEncoding.EncodedLen(mail.MaxMessageBytes) {
		return mail.MutationResult{}, mail.NewError("too_large", "Message exceeds the 10 MiB append limit")
	}
	data, err := base64.StdEncoding.DecodeString(q.MessageBase64)
	if err != nil {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "Message must be valid base64")
	}
	if len(data) == 0 {
		return mail.MutationResult{}, mail.NewError("invalid_argument", "Message must not be empty")
	}
	if len(data) > mail.MaxMessageBytes {
		return mail.MutationResult{}, mail.NewError("too_large", "Message exceeds the 10 MiB append limit")
	}
	flags, err := mutationFlags(q.Flags, true)
	if err != nil {
		return mail.MutationResult{}, err
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.MutationResult{}, err
	}
	defer s.close()
	opts := &imap.AppendOptions{Flags: flags}
	if q.InternalDate != nil {
		opts.Time = *q.InternalDate
	}
	cmd := s.c.Append(q.Mailbox, int64(len(data)), opts)
	_, writeErr := cmd.Write(data)
	closeErr := cmd.Close()
	appended, waitErr := cmd.Wait()
	if writeErr != nil {
		return mail.MutationResult{}, s.err(writeErr, true)
	}
	if closeErr != nil {
		return mail.MutationResult{}, s.err(closeErr, true)
	}
	if waitErr != nil {
		return mail.MutationResult{}, s.err(waitErr, true)
	}
	result := mail.MutationResult{Affected: []mail.MessageRef{}, DestinationRefs: []mail.MessageRef{}}
	if appended.UIDValidity != 0 && appended.UID != 0 {
		result.DestinationRefs = []mail.MessageRef{{Mailbox: q.Mailbox, UIDValidity: appended.UIDValidity, UID: uint32(appended.UID)}}
		result.MappingAvailable = true
	}
	return result, nil
}
func (c *Client) CreateMailbox(ctx context.Context, name string) error {
	if err := c.writeGuard(); err != nil {
		return err
	}
	if name == "" {
		return mail.NewError("invalid_argument", "Mailbox is required")
	}
	s, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	return s.err(s.c.Create(name, nil).Wait(), true)
}
