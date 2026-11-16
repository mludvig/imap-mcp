package client

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/mludvig/imap-mcp/internal/mail"
)

// Bound traversal by the selected mailbox count and use uint64 iteration so a
// range ending at 4294967295 terminates; see docs/concepts/imap-safety.md.
func expandUIDs(set imap.UIDSet, max uint64) ([]uint32, error) {
	result := []uint32{}
	var count uint64
	for _, item := range set {
		start, stop := uint64(item.Start), uint64(item.Stop)
		if start == 0 || stop == 0 {
			return nil, mail.NewError("server_error", "IMAP server returned a dynamic UID set")
		}
		if start > stop {
			start, stop = stop, start
		}
		count += stop - start + 1
		if count > max {
			return nil, mail.NewError("server_error", "IMAP server returned an oversized UID set")
		}
		for uid := start; uid <= stop; uid++ {
			result = append(result, uint32(uid))
		}
	}
	return result, nil
}

func searchCriteria(q mail.SearchQuery) (*imap.SearchCriteria, error) {
	result := &imap.SearchCriteria{Larger: q.MinSize, Smaller: q.MaxSize}
	if q.MinSize < 0 || q.MaxSize < 0 {
		return nil, mail.NewError("invalid_argument", "Message size constraints must be nonnegative")
	}
	for _, field := range []struct{ name, value string }{{"From", q.From}, {"To", q.To}, {"Cc", q.Cc}, {"Bcc", q.Bcc}, {"Subject", q.Subject}} {
		if field.value != "" {
			result.Header = append(result.Header, imap.SearchCriteriaHeaderField{Key: field.name, Value: field.value})
		}
	}
	keys := make([]string, 0, len(q.Headers))
	for name := range q.Headers {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if name == "" || strings.ContainsAny(name, "\r\n:") {
			return nil, mail.NewError("invalid_argument", "Invalid search header name")
		}
		result.Header = append(result.Header, imap.SearchCriteriaHeaderField{Key: name, Value: q.Headers[name]})
	}
	if q.Body != "" {
		result.Body = []string{q.Body}
	}
	if q.Text != "" {
		result.Text = []string{q.Text}
	}
	dates := []struct {
		input  string
		target *time.Time
	}{{q.Since, &result.Since}, {q.Before, &result.Before}, {q.SentSince, &result.SentSince}, {q.SentBefore, &result.SentBefore}}
	for _, date := range dates {
		if date.input == "" {
			continue
		}
		value, err := time.Parse("2006-01-02", date.input)
		if err != nil {
			return nil, mail.NewError("invalid_argument", "Search dates must use YYYY-MM-DD")
		}
		*date.target = value
	}
	var err error
	result.Flag, err = parseFlags(q.Flags, false)
	if err != nil {
		return nil, err
	}
	result.NotFlag, err = parseFlags(q.NotFlags, false)
	if err != nil {
		return nil, err
	}
	if q.BeforeUID != 0 {
		// UID 1 is an empty cursor, handled without issuing a reversed IMAP range.
		if q.BeforeUID > 1 {
			result.UID = []imap.UIDSet{{{Start: 1, Stop: imap.UID(q.BeforeUID - 1)}}}
		}
	}
	return result, nil
}
func (s *session) search(criteria *imap.SearchCriteria, max uint64) ([]uint32, error) {
	options := &imap.SearchOptions{}
	if s.caps.Has(imap.CapESearch) {
		options.ReturnMin = true
		options.ReturnMax = true
		options.ReturnAll = true
		options.ReturnCount = true
	}
	data, err := s.c.UIDSearch(criteria, options).Wait()
	if err != nil {
		return nil, s.err(err, false)
	}
	if data.All == nil {
		return []uint32{}, nil
	}
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil, mail.NewError("server_error", "IMAP UID SEARCH returned sequence numbers")
	}
	return expandUIDs(set, max)
}
func (c *Client) SearchUIDs(ctx context.Context, q mail.SearchQuery) ([]uint32, error) {
	criteria, err := searchCriteria(q)
	if err != nil {
		return nil, err
	}
	if q.BeforeUID == 1 {
		return []uint32{}, nil
	}
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	selected, err := s.selectMailbox(q.Mailbox, true)
	if err != nil {
		return nil, err
	}
	var result []uint32
	ordering := strings.ToLower(q.Sort)
	switch ordering {
	case "", "uid", "uid_desc", "uid_asc":
		result, err = s.search(criteria, uint64(selected.NumMessages))
		sort.Slice(result, func(i, j int) bool {
			if ordering == "uid_asc" {
				return result[i] < result[j]
			}
			return result[i] > result[j]
		})
	default:
		if !s.caps.Has(imap.CapSort) {
			return nil, mail.NewError("unsupported", "The IMAP server does not support SORT")
		}
		key := ordering
		reverse := strings.HasSuffix(key, "_desc")
		key = strings.TrimSuffix(strings.TrimSuffix(key, "_desc"), "_asc")
		switch key {
		case "arrival", "date", "from", "to", "cc", "subject", "size":
		default:
			return nil, mail.NewError("invalid_argument", "Unsupported sort key")
		}
		result, err = s.c.UIDSort(&imapclient.SortOptions{SearchCriteria: criteria, SortCriteria: []imapclient.SortCriterion{{Key: imapclient.SortKey(strings.ToUpper(key)), Reverse: reverse}}}).Wait()
		if err != nil {
			err = s.err(err, false)
		}
	}
	if err != nil {
		return nil, err
	}
	for _, uid := range result {
		if uid == 0 {
			return nil, mail.NewError("server_error", "IMAP server returned UID zero")
		}
	}
	if _, err := s.finalEpoch(q.Mailbox, selected.UIDValidity); err != nil {
		return nil, err
	}
	return result, nil
}

func convertThreads(data []imapclient.ThreadData, name string, epoch uint32) ([]mail.ThreadNode, error) {
	result := []mail.ThreadNode{}
	for _, thread := range data {
		children, err := convertThreads(thread.SubThreads, name, epoch)
		if err != nil {
			return nil, err
		}
		for i := len(thread.Chain) - 1; i >= 0; i-- {
			if thread.Chain[i] == 0 {
				return nil, mail.NewError("server_error", "IMAP THREAD returned UID zero")
			}
			children = []mail.ThreadNode{{Ref: mail.MessageRef{Mailbox: name, UIDValidity: epoch, UID: thread.Chain[i]}, Children: children}}
		}
		result = append(result, children...)
	}
	return result, nil
}
func (c *Client) ThreadMessages(ctx context.Context, q mail.ThreadRequest) (mail.ThreadResult, error) {
	if q.Query.Mailbox != "" && q.Query.Mailbox != q.Mailbox {
		return mail.ThreadResult{}, mail.NewError("invalid_argument", "Thread query mailbox must match the requested mailbox")
	}
	criteria, err := searchCriteria(q.Query)
	if err != nil {
		return mail.ThreadResult{}, err
	}
	if q.Query.BeforeUID == 1 {
		return mail.ThreadResult{Threads: []mail.ThreadNode{}}, nil
	}
	s, err := c.open(ctx)
	if err != nil {
		return mail.ThreadResult{}, err
	}
	defer s.close()
	algorithm := imap.ThreadAlgorithm(strings.ToUpper(q.Algorithm))
	if algorithm == "" {
		algorithm = imap.ThreadReferences
	}
	supported := false
	for _, available := range s.caps.ThreadAlgorithms() {
		if available == algorithm {
			supported = true
		}
	}
	if !supported {
		return mail.ThreadResult{}, mail.NewError("unsupported", "The IMAP server does not support the requested THREAD algorithm")
	}
	selected, err := s.selectMailbox(q.Mailbox, true)
	if err != nil {
		return mail.ThreadResult{}, err
	}
	data, err := s.c.UIDThread(&imapclient.ThreadOptions{Algorithm: algorithm, SearchCriteria: criteria}).Wait()
	if err != nil {
		return mail.ThreadResult{}, s.err(err, false)
	}
	threads, err := convertThreads(data, q.Mailbox, selected.UIDValidity)
	if err != nil {
		return mail.ThreadResult{}, err
	}
	if _, err := s.finalEpoch(q.Mailbox, selected.UIDValidity); err != nil {
		return mail.ThreadResult{}, err
	}
	return mail.ThreadResult{Threads: threads}, nil
}
