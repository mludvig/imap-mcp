package cache

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/mludvig/imap-mcp/internal/mail"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Query applies AND predicates to cached headers only. Comparisons are literal
// substrings after NFC normalization and Unicode case folding, never SQL LIKE.
// Only descending UID is pageable because the cursor is an exclusive upper UID.
func (c *Cache) Query(ctx context.Context, key mail.MailboxKey, q mail.SearchQuery) (mail.MessagePage, error) {
	for _, flags := range [][]string{q.Flags, q.NotFlags} {
		for _, flag := range flags {
			if strings.EqualFold(flag, "\\Recent") {
				return mail.MessagePage{}, mail.NewError("invalid_argument", "Searching the Recent flag is not supported")
			}
		}
	}
	key, err := canonicalKey(key)
	if err != nil {
		return mail.MessagePage{}, err
	}
	if q.Mailbox != "" {
		queryKey, err := canonicalKey(mail.MailboxKey{Account: key.Account, Name: q.Mailbox})
		if err != nil || queryKey != key {
			return mail.MessagePage{}, mail.NewError("invalid_argument", "query mailbox differs from cache mailbox")
		}
	}
	if q.Body != "" || q.Text != "" {
		return mail.MessagePage{}, mail.NewError("unsupported", "body and text searches require live IMAP search")
	}
	if q.Limit == 0 {
		q.Limit = mail.DefaultLimit
	}
	if q.Limit < 1 || q.Limit > mail.MaxLimit || q.MinSize < 0 || q.MaxSize < 0 {
		return mail.MessagePage{}, mail.NewError("invalid_argument", "invalid search limit or size bound")
	}
	q.Sort = strings.ToLower(q.Sort)
	if q.Sort == "" {
		q.Sort = "uid"
	}
	field := strings.TrimSuffix(q.Sort, "_desc")
	if field == q.Sort {
		field = strings.TrimSuffix(q.Sort, "_asc")
	}
	descending := strings.HasSuffix(q.Sort, "_desc") || (field == "uid" && !strings.HasSuffix(q.Sort, "_asc"))
	switch field {
	case "uid", "date", "arrival", "subject", "from", "size":
	default:
		return mail.MessagePage{}, mail.NewError("unsupported", "unsupported cached sort field")
	}
	pageable := field == "uid" && descending
	if q.BeforeUID != 0 && !pageable {
		return mail.MessagePage{}, mail.NewError("invalid_argument", "before_uid requires descending UID sort")
	}
	for _, value := range []string{q.Since, q.Before, q.SentSince, q.SentBefore} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil || len(value) != 10 {
				return mail.MessagePage{}, mail.NewError("invalid_argument", "date constraints must use YYYY-MM-DD")
			}
		}
	}
	for name := range q.Headers {
		if name == "" {
			return mail.MessagePage{}, mail.NewError("invalid_argument", "invalid search header name")
		}
		for _, r := range name {
			if r <= 32 || r > 126 || r == ':' {
				return mail.MessagePage{}, mail.NewError("invalid_argument", "invalid search header name")
			}
		}
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mail.MessagePage{}, err
	}
	defer tx.Rollback()
	state, err := mailboxState(ctx, tx, key)
	if err != nil {
		return mail.MessagePage{}, err
	}
	records, err := readRecords(ctx, tx, key)
	if err != nil {
		return mail.MessagePage{}, err
	}
	if err := tx.Commit(); err != nil {
		return mail.MessagePage{}, err
	}
	page := mail.MessagePage{Messages: []mail.MessageSummary{}, UIDValidity: state.UIDValidity}
	for uid, item := range records {
		if err := ctx.Err(); err != nil {
			return mail.MessagePage{}, err
		}
		item.summary.Flags = persistentFlags(item.summary.Flags)
		if !matches(item, q) {
			continue
		}
		page.Total++
		if q.BeforeUID == 0 || uid < q.BeforeUID {
			page.Messages = append(page.Messages, item.summary)
		}
	}
	sort.Slice(page.Messages, func(i, j int) bool {
		a, b := page.Messages[i], page.Messages[j]
		if !descending {
			a, b = b, a
		}
		switch field {
		case "date":
			var ad, bd time.Time
			if a.Date != nil {
				ad = *a.Date
			}
			if b.Date != nil {
				bd = *b.Date
			}
			if !ad.Equal(bd) {
				return ad.After(bd)
			}
		case "arrival":
			if !a.InternalDate.Equal(b.InternalDate) {
				return a.InternalDate.After(b.InternalDate)
			}
		case "size":
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		case "subject":
			if av, bv := folded(a.Subject), folded(b.Subject); av != bv {
				return av > bv
			}
		case "from":
			if av, bv := folded(strings.Join(a.From, ", ")), folded(strings.Join(b.From, ", ")); av != bv {
				return av > bv
			}
		}
		return a.Ref.UID > b.Ref.UID
	})
	if len(page.Messages) > q.Limit {
		page.Messages = page.Messages[:q.Limit]
		if pageable {
			page.NextBeforeUID = page.Messages[len(page.Messages)-1].Ref.UID
		}
	}
	return page, nil
}

func folded(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

func containsHeader(headers map[string][]string, name, needle string) bool {
	values, present := headers[strings.ToLower(name)]
	if needle == "" {
		return present // An empty criterion means presence, including empty fields.
	}
	needle = folded(needle)
	for _, value := range values {
		if strings.Contains(folded(value), needle) {
			return true
		}
	}
	return false
}

func matches(item record, q mail.SearchQuery) bool {
	for name, needle := range map[string]string{"from": q.From, "to": q.To, "cc": q.Cc, "bcc": q.Bcc, "subject": q.Subject} {
		if needle != "" && !containsHeader(item.headers, name, needle) {
			return false
		}
	}
	for name, needle := range q.Headers {
		if !containsHeader(item.headers, name, needle) {
			return false
		}
	}
	s := item.summary
	// Match IMAP LARGER and SMALLER: both size bounds are strict.
	if (q.MinSize != 0 && s.Size <= q.MinSize) || (q.MaxSize != 0 && s.Size >= q.MaxSize) {
		return false
	}
	// Format uses the original offset, so these are civil dates, not UTC dates.
	if !dateMatches(s.InternalDate, q.Since, q.Before) {
		return false
	}
	if q.SentSince != "" || q.SentBefore != "" {
		if s.Date == nil || !dateMatches(*s.Date, q.SentSince, q.SentBefore) {
			return false
		}
	}
	for _, flag := range q.Flags {
		if !hasFlag(s.Flags, flag) {
			return false
		}
	}
	for _, flag := range q.NotFlags {
		if hasFlag(s.Flags, flag) {
			return false
		}
	}
	return true
}

func dateMatches(date time.Time, since, before string) bool {
	civil := date.Format("2006-01-02")
	return (since == "" || civil >= since) && (before == "" || civil < before)
}

// IMAP flags and keywords ignore case; stored spelling is preserved.
// See docs/concepts/cache-coherence.md for the boundary with live IMAP searches.
func hasFlag(flags []string, wanted string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, wanted) {
			return true
		}
	}
	return false
}
