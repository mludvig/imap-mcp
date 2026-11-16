package service

import (
	"strings"
	"time"

	"github.com/mludvig/imap-mcp/internal/mail"
)

func invalid(message string) error { return mail.NewError("invalid_argument", message) }

func validateQuery(q mail.SearchQuery) (mail.SearchQuery, error) {
	q.Mailbox = mailbox(q.Mailbox)
	if q.Limit == 0 {
		q.Limit = mail.DefaultLimit
	}
	if q.Limit < 1 || q.Limit > mail.MaxLimit {
		return q, invalid("Search limit must be between 1 and 200")
	}
	if q.MinSize < 0 || q.MaxSize < 0 {
		return q, invalid("Message size constraints must be nonnegative")
	}
	q.Sort = strings.ToLower(q.Sort)
	if q.Sort == "" || q.Sort == "uid" {
		q.Sort = "uid_desc"
	}
	field := strings.TrimSuffix(strings.TrimSuffix(q.Sort, "_asc"), "_desc")
	switch field {
	case "uid", "arrival", "date", "from", "to", "cc", "subject", "size":
	default:
		return q, invalid("Unsupported sort key")
	}
	if q.Sort != field && q.Sort != field+"_asc" && q.Sort != field+"_desc" {
		return q, invalid("Unsupported sort key")
	}
	if q.BeforeUID != 0 && q.Sort != "uid_desc" {
		return q, invalid("before_uid requires descending UID sort")
	}
	for _, date := range []string{q.Since, q.Before, q.SentSince, q.SentBefore} {
		if date == "" {
			continue
		}
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil || len(date) != 10 || parsed.Format("2006-01-02") != date {
			return q, invalid("Search dates must use YYYY-MM-DD")
		}
	}
	for name := range q.Headers {
		if name == "" {
			return q, invalid("Invalid search header name")
		}
		for _, r := range name {
			if r < 33 || r > 126 || r == ':' {
				return q, invalid("Invalid search header name")
			}
		}
	}
	for _, flags := range [][]string{q.Flags, q.NotFlags} {
		if err := validateFlags(flags, false, false); err != nil {
			return q, err
		}
		for _, flag := range flags {
			if strings.EqualFold(flag, "\\Recent") {
				return q, invalid("Searching the Recent flag is not supported")
			}
		}
	}
	return q, nil
}

func validateFlags(flags []string, mutation, adding bool) error {
	for _, flag := range flags {
		if flag == "" {
			return invalid("Flag must not be empty")
		}
		atom := flag
		if strings.HasPrefix(atom, "\\") {
			switch strings.ToLower(flag) {
			case "\\seen", "\\answered", "\\flagged", "\\deleted", "\\draft", "\\recent":
			default:
				return invalid("Unknown system flag")
			}
			atom = atom[1:]
		}
		for _, r := range atom {
			if r < 33 || r > 126 || strings.ContainsRune("(){%*\"\\]}", r) {
				return invalid("Flag must be an IMAP atom")
			}
		}
		if mutation && strings.EqualFold(flag, "\\Recent") {
			return invalid("The Recent flag cannot be changed")
		}
		if adding && strings.EqualFold(flag, "\\Deleted") {
			return invalid("Adding the Deleted flag is forbidden")
		}
	}
	return nil
}

func validateRef(ref mail.MessageRef) error {
	if ref.Mailbox == "" || ref.UIDValidity == 0 || ref.UID == 0 {
		return invalid("A complete message reference is required")
	}
	return nil
}

func validateRefs(refs []mail.MessageRef) ([]mail.MessageRef, error) {
	if len(refs) < 1 || len(refs) > mail.MaxLimit {
		return nil, invalid("Between 1 and 200 message references are required")
	}
	result := append([]mail.MessageRef(nil), refs...)
	seen := make(map[uint32]bool, len(refs))
	for i, ref := range refs {
		if err := validateRef(ref); err != nil {
			return nil, err
		}
		result[i].Mailbox = mailbox(ref.Mailbox)
		if result[i].Mailbox != mailbox(refs[0].Mailbox) || ref.UIDValidity != refs[0].UIDValidity {
			return nil, invalid("Message references must share one mailbox and UIDVALIDITY")
		}
		if seen[ref.UID] {
			return nil, invalid("Duplicate message reference")
		}
		seen[ref.UID] = true
	}
	return result, nil
}
