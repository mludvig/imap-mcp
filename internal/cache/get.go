package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mludvig/imap-mcp/internal/mail"
)

// Get retrieves summaries in UID order supplied by a live BODY/TEXT search or
// native SORT. Only service pages are supported, bounded by mail.MaxLimit.
// See docs/concepts/cache-coherence.md: refresh must succeed before cache use.
func (c *Cache) Get(ctx context.Context, key mail.MailboxKey, uids []uint32) ([]mail.MessageSummary, error) {
	key, err := canonicalKey(key)
	if err != nil {
		return nil, err
	}
	if len(uids) > mail.MaxLimit {
		return nil, mail.NewError("invalid_argument", "cache lookup exceeds page limit")
	}
	seen := make(map[uint32]bool, len(uids))
	for _, uid := range uids {
		if uid == 0 || seen[uid] {
			return nil, mail.NewError("invalid_argument", "cache lookup contains a zero or duplicate UID")
		}
		seen[uid] = true
	}
	summaries := make([]mail.MessageSummary, 0, len(uids))
	if len(uids) == 0 {
		return summaries, nil
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	state, err := mailboxState(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if !state.Present || state.UIDValidity == 0 {
		return nil, mail.NewError("cache_miss", "cache mailbox has no complete epoch")
	}
	for _, uid := range uids {
		var encoded []byte
		err := tx.QueryRowContext(ctx, `SELECT summary FROM messages WHERE account=? AND mailbox=? AND uid=?`, key.Account, key.Name, uid).Scan(&encoded)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, mail.NewError("cache_miss", "requested UID is absent from cache")
		}
		if err != nil {
			return nil, err
		}
		var summary mail.MessageSummary
		if err := json.Unmarshal(encoded, &summary); err != nil {
			return nil, err
		}
		summary.Flags = persistentFlags(summary.Flags)
		summaries = append(summaries, summary)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return summaries, nil
}
