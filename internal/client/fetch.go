package client

import (
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mludvig/imap-mcp/internal/mail"
)

type fetched struct {
	seq, uid                                uint32
	flags                                   []string
	modseq                                  uint64
	size                                    int64
	date                                    time.Time
	body                                    []byte
	haveSize, haveDate, haveBody, haveFlags bool
}

// Sequence FETCH avoids beta.9's UID-before-literal assumption. Consumers must
// verify the returned UID; see docs/concepts/imap-safety.md.
func (s *session) fetch(set imap.NumSet, opts *imap.FetchOptions, section *imap.FetchItemBodySection, maxBytes int64) ([]fetched, error) {
	cmd := s.c.Fetch(set, opts)
	defer cmd.Close()
	result := []fetched{}
	var failure error
	for message := cmd.Next(); message != nil; message = cmd.Next() {
		item := fetched{seq: message.SeqNum}
		for data := message.Next(); data != nil; data = message.Next() {
			switch data := data.(type) {
			case imapclient.FetchItemDataUID:
				item.uid = uint32(data.UID)
			case imapclient.FetchItemDataFlags:
				// Recent is session state, outside MODSEQ tracking; see
				// docs/concepts/cache-coherence.md for persistent flag semantics.
				item.flags = make([]string, 0, len(data.Flags))
				for _, flag := range data.Flags {
					if !strings.EqualFold(string(flag), "\\Recent") {
						item.flags = append(item.flags, string(flag))
					}
				}
				item.haveFlags = true
			case imapclient.FetchItemDataModSeq:
				item.modseq = data.ModSeq
			case imapclient.FetchItemDataRFC822Size:
				item.size = data.Size
				item.haveSize = true
			case imapclient.FetchItemDataInternalDate:
				item.date = data.Time
				item.haveDate = true
			case imapclient.FetchItemDataBinarySection:
				// BINARY retrieval is not exposed, but unsolicited literals
				// still require the same bounded cleanup as BODY literals.
				if data.Literal == nil {
					continue
				}
				if data.Literal.Size() > maxBytes {
					s.abandonLiteral(data.Literal)
					failure = mail.NewError("too_large", "IMAP literal exceeds the size limit")
				} else if _, err := io.Copy(io.Discard, data.Literal); err != nil {
					s.failedLiteral()
					if failure == nil {
						failure = s.err(err, false)
					}
				}
			case imapclient.FetchItemDataBodySection:
				if data.Literal == nil {
					continue
				}
				if section == nil || !data.MatchCommand(section) {
					// Unrequested literals must also be bounded and consumed.
					if data.Literal.Size() > maxBytes {
						s.abandonLiteral(data.Literal)
						failure = mail.NewError("too_large", "IMAP literal exceeds the size limit")
					} else if _, err := io.Copy(io.Discard, data.Literal); err != nil {
						s.failedLiteral()
						if failure == nil {
							failure = s.err(err, false)
						}
					}
					continue
				}
				if data.Literal.Size() < 0 || data.Literal.Size() > maxBytes {
					s.abandonLiteral(data.Literal)
					failure = mail.NewError("too_large", "IMAP literal exceeds the size limit")
					continue
				}
				body, err := io.ReadAll(io.LimitReader(data.Literal, maxBytes+1))
				if err != nil {
					s.failedLiteral()
					if failure == nil {
						failure = s.err(err, false)
					}
					continue
				}
				if int64(len(body)) > maxBytes {
					s.failedLiteral()
					failure = mail.NewError("too_large", "IMAP literal exceeds the size limit")
					continue
				}
				item.body = body
				item.haveBody = true
			}
		}
		result = append(result, item)
	}
	if err := cmd.Close(); failure == nil && err != nil {
		failure = s.err(err, false)
	}
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

// beta.9 releases its decoder when a literal read fails but Next retries the
// literal discard. Wait before that retry to avoid concurrent bufio access.
// See docs/concepts/imap-safety.md.
func (s *session) failedLiteral() {
	_ = s.raw.Close()
	<-s.c.Closed()
}
func (s *session) abandonLiteral(literal imap.LiteralReader) {
	_ = s.raw.Close()
	_, _ = io.Copy(io.Discard, literal)
	<-s.c.Closed()
}

func (s *session) membership(count uint32) (map[uint32]uint32, error) {
	members := make(map[uint32]uint32)
	if count == 0 {
		return members, nil
	}
	result, err := s.fetch(imap.SeqSet{{Start: 1, Stop: count}}, &imap.FetchOptions{UID: true}, nil, mail.MaxHeaderBytes)
	if err != nil {
		return nil, err
	}
	for _, item := range result {
		if item.uid == 0 || item.seq == 0 {
			return nil, mail.NewError("server_error", "IMAP membership response is incomplete")
		}
		if _, exists := members[item.uid]; exists {
			return nil, mail.NewError("server_error", "IMAP membership response contains duplicate UIDs")
		}
		members[item.uid] = item.seq
	}
	return members, nil
}
func sortedMembers(members map[uint32]uint32) []uint32 {
	result := make([]uint32, 0, len(members))
	for uid := range members {
		result = append(result, uid)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
func uidSet(uids []uint32) imap.UIDSet {
	result := imap.UIDSet{}
	for _, uid := range uids {
		result.AddNum(imap.UID(uid))
	}
	return result
}
func (s *session) finalEpoch(name string, epoch uint32) (*imap.StatusData, error) {
	status, err := s.status(name)
	if err != nil {
		return nil, s.err(err, false)
	}
	if status.UIDValidity != epoch {
		return nil, mail.NewError("stale_reference", "Mailbox UIDVALIDITY changed during the operation")
	}
	return status, nil
}

func (c *Client) Refresh(ctx context.Context, name string, cached mail.CacheState) (mail.Refresh, error) {
	s, err := c.open(ctx)
	if err != nil {
		return mail.Refresh{}, err
	}
	defer s.close()
	selected, err := s.selectMailbox(name, true)
	if err != nil {
		return mail.Refresh{}, err
	}
	members, err := s.membership(selected.NumMessages)
	if err != nil {
		return mail.Refresh{}, err
	}
	known := map[uint32]bool{}
	cachedUIDs := map[uint32]bool{}
	matching := cached.Present && cached.UIDValidity == selected.UIDValidity
	if matching {
		for _, uid := range cached.CachedUIDs {
			known[uid] = true
			cachedUIDs[uid] = true
		}
	}
	result := mail.Refresh{UIDValidity: selected.UIDValidity, HighestModSeq: selected.HighestModSeq, NewMetadata: []mail.Metadata{}, FlagUpdates: []mail.FlagUpdate{}}
	// Persist the SELECT start cursor, never the maximum observed FETCH modseq.
	// See docs/concepts/imap-safety.md.
	modseq := s.caps.Has(imap.CapCondStore) && selected.HighestModSeq != 0
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	newUIDs := []uint32{}
	sequences := imap.SeqSet{}
	for _, uid := range sortedMembers(members) {
		if !known[uid] {
			newUIDs = append(newUIDs, uid)
			sequences.AddNum(members[uid])
		}
	}
	if len(newUIDs) != 0 {
		data, err := s.fetch(sequences, &imap.FetchOptions{UID: true, RFC822Size: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{section}}, section, mail.MaxHeaderBytes)
		if err != nil {
			return mail.Refresh{}, err
		}
		requested := map[uint32]bool{}
		for _, uid := range newUIDs {
			requested[uid] = true
		}
		for _, item := range data {
			if !requested[item.uid] || members[item.uid] != item.seq {
				return mail.Refresh{}, mail.NewError("server_error", "Mailbox membership changed during header fetch")
			}
			if !item.haveBody || !item.haveDate || !item.haveSize {
				return mail.Refresh{}, mail.NewError("server_error", "IMAP header response is incomplete")
			}
			result.NewMetadata = append(result.NewMetadata, mail.Metadata{UID: item.uid, Header: item.body, InternalDate: item.date, Size: item.size})
			known[item.uid] = true
		}
	}
	uids := sortedMembers(members)
	gotFlags := map[uint32]bool{}
	requireFlags := map[uint32]bool{}
	if len(uids) != 0 {
		opts := &imap.FetchOptions{UID: true, Flags: true, ModSeq: modseq}
		if modseq && matching && cached.HighestModSeq > 0 && cached.HighestModSeq <= selected.HighestModSeq {
			opts.ChangedSince = cached.HighestModSeq
		}
		flags, err := s.fetch(uidSet(uids), opts, nil, mail.MaxHeaderBytes)
		if err != nil {
			return mail.Refresh{}, err
		}
		for _, item := range flags {
			if _, present := members[item.uid]; !present {
				continue
			}
			if !item.haveFlags {
				return mail.Refresh{}, mail.NewError("server_error", "IMAP flag response is incomplete")
			}
			result.FlagUpdates = append(result.FlagUpdates, mail.FlagUpdate{UID: item.uid, Flags: item.flags, ModSeq: item.modseq})
			gotFlags[item.uid] = true
		}
		// Newly arrived messages must have flags even if the server's CHANGEDSINCE
		// response omits them. A targeted full FLAGS fetch is sufficient.
		missing := []uint32{}
		for _, uid := range uids {
			if opts.ChangedSince == 0 || !cachedUIDs[uid] {
				requireFlags[uid] = true
			}
			if !gotFlags[uid] && requireFlags[uid] {
				missing = append(missing, uid)
			}
		}
		if len(missing) != 0 {
			flags, err := s.fetch(uidSet(missing), &imap.FetchOptions{UID: true, Flags: true, ModSeq: modseq}, nil, mail.MaxHeaderBytes)
			if err != nil {
				return mail.Refresh{}, err
			}
			for _, item := range flags {
				if item.haveFlags && requireFlags[item.uid] {
					gotFlags[item.uid] = true
					result.FlagUpdates = append(result.FlagUpdates, mail.FlagUpdate{UID: item.uid, Flags: item.flags, ModSeq: item.modseq})
				}
			}
		}
	}
	status, err := s.finalEpoch(name, selected.UIDValidity)
	if err != nil {
		return mail.Refresh{}, err
	}
	if status.NumMessages == nil {
		return mail.Refresh{}, mail.NewError("server_error", "IMAP status response is incomplete")
	}
	finalMembers, err := s.membership(*status.NumMessages)
	if err != nil {
		return mail.Refresh{}, err
	}
	for uid := range finalMembers {
		if !known[uid] || (requireFlags[uid] && !gotFlags[uid]) {
			return mail.Refresh{}, mail.NewError("server_error", "Mailbox changed during refresh; refresh again")
		}
	}
	verified, err := s.finalEpoch(name, selected.UIDValidity)
	if err != nil {
		return mail.Refresh{}, err
	}
	if verified.NumMessages == nil || *verified.NumMessages != uint32(len(finalMembers)) || verified.UIDNext != status.UIDNext {
		return mail.Refresh{}, mail.NewError("server_error", "Mailbox changed during refresh; refresh again")
	}
	metadata := result.NewMetadata[:0]
	for _, item := range result.NewMetadata {
		if _, present := finalMembers[item.UID]; present {
			metadata = append(metadata, item)
		}
	}
	result.NewMetadata = metadata
	flags := result.FlagUpdates[:0]
	for _, item := range result.FlagUpdates {
		if _, present := finalMembers[item.UID]; present {
			flags = append(flags, item)
		}
	}
	result.FlagUpdates = flags
	result.Members = sortedMembers(finalMembers)
	return result, nil
}
func (s *session) rawMessage(ref mail.MessageRef) (mail.Metadata, []string, uint64, []byte, error) {
	selected, err := s.selectMailbox(ref.Mailbox, true)
	if err != nil {
		return mail.Metadata{}, nil, 0, nil, err
	}
	if err := checkRef(ref, selected.UIDValidity); err != nil {
		return mail.Metadata{}, nil, 0, nil, err
	}
	data, err := s.fetch(imap.UIDSetNum(imap.UID(ref.UID)), &imap.FetchOptions{UID: true, RFC822Size: true, InternalDate: true, Flags: true, ModSeq: selected.HighestModSeq != 0}, nil, mail.MaxHeaderBytes)
	if err != nil {
		return mail.Metadata{}, nil, 0, nil, err
	}
	if len(data) != 1 || data[0].uid != ref.UID {
		return mail.Metadata{}, nil, 0, nil, mail.NewError("not_found", "Message no longer exists")
	}
	info := data[0]
	if !info.haveSize || !info.haveDate || !info.haveFlags {
		return mail.Metadata{}, nil, 0, nil, mail.NewError("server_error", "IMAP message metadata is incomplete")
	}
	if info.size < 0 || info.size > mail.MaxMessageBytes {
		return mail.Metadata{}, nil, 0, nil, mail.NewError("too_large", "Message exceeds the 10 MiB read limit")
	}
	section := &imap.FetchItemBodySection{Peek: true}
	data, err = s.fetch(imap.SeqSetNum(info.seq), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}, section, mail.MaxMessageBytes)
	if err != nil {
		return mail.Metadata{}, nil, 0, nil, err
	}
	if len(data) != 1 || data[0].uid != ref.UID || !data[0].haveBody {
		return mail.Metadata{}, nil, 0, nil, mail.NewError("not_found", "Message changed during body fetch")
	}
	if _, err := s.finalEpoch(ref.Mailbox, ref.UIDValidity); err != nil {
		return mail.Metadata{}, nil, 0, nil, err
	}
	return mail.Metadata{UID: ref.UID, InternalDate: info.date, Size: info.size}, info.flags, info.modseq, data[0].body, nil
}
