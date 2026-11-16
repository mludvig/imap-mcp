// Package cache stores only headers and metadata, never message bodies.
// See docs/concepts/cache-coherence.md for refresh and invalidation invariants.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gofrs/flock"
	"github.com/mludvig/imap-mcp/internal/diagnostics"
	"github.com/mludvig/imap-mcp/internal/mail"
	_ "modernc.org/sqlite"
)

type Cache struct {
	db       *sql.DB
	lock     *flock.Flock
	close    sync.Once
	closeErr error
}

// Open takes an exclusive process lock before accessing the private database.
// Existing parent directories retain their owner's chosen permissions.
func Open(path string) (*Cache, error) {
	return OpenContext(context.Background(), path)
}

// OpenContext attaches cache initialization and rebuild diagnostics to ctx.
func OpenContext(ctx context.Context, path string) (*Cache, error) {
	diagnostics.Stage(ctx, "Opening header cache")
	defer diagnostics.Measure(ctx, "cache open")()
	if path == "" || path == ":memory:" {
		return nil, errors.New("cache requires a persistent file path")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve cache path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if err := privateFile(path + ".lock"); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock cache: %w", err)
	}
	if !locked {
		return nil, errors.New("cache is already owned by another process")
	}
	c := &Cache{lock: lock}
	if err := privateFile(path); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	// Escape URI delimiters so a literal filename cannot become driver options.
	dsn := "file:" + strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	c.db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	// One connection also keeps PRAGMA settings and multi-query snapshots coherent.
	c.db.SetMaxOpenConns(1)
	err = c.initialize(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("initialize cache: %w", err), c.Close())
	}
	return c, nil
}

func privateFile(path string) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("cache and lock paths must be regular files")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	return errors.Join(f.Chmod(0600), f.Close())
}

func (c *Cache) Close() error {
	c.close.Do(func() {
		if c.db != nil {
			c.closeErr = c.db.Close()
		}
		if c.lock != nil {
			c.closeErr = errors.Join(c.closeErr, c.lock.Close())
		}
	})
	return c.closeErr
}

func canonicalKey(key mail.MailboxKey) (mail.MailboxKey, error) {
	if key.Account == "" || key.Name == "" {
		return key, mail.NewError("invalid_argument", "cache account and mailbox are required")
	}
	if strings.EqualFold(key.Name, "INBOX") {
		key.Name = "INBOX"
	}
	return key, nil
}

func mailboxState(ctx context.Context, tx *sql.Tx, key mail.MailboxKey) (mail.CacheState, error) {
	state := mail.CacheState{CachedUIDs: []uint32{}}
	var modSeq string
	err := tx.QueryRowContext(ctx, `SELECT uid_validity, highest_mod_seq FROM mailboxes WHERE account=? AND name=?`, key.Account, key.Name).Scan(&state.UIDValidity, &modSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.HighestModSeq, err = strconv.ParseUint(modSeq, 10, 64)
	if err != nil {
		return state, fmt.Errorf("invalid cached modification sequence: %w", err)
	}
	state.Present = true
	return state, nil
}

func (c *Cache) State(ctx context.Context, key mail.MailboxKey) (mail.CacheState, error) {
	key, err := canonicalKey(key)
	if err != nil {
		return mail.CacheState{}, err
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return mail.CacheState{}, err
	}
	defer tx.Rollback()
	state, err := mailboxState(ctx, tx, key)
	if err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT uid FROM messages WHERE account=? AND mailbox=? ORDER BY uid`, key.Account, key.Name)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			rows.Close()
			return state, err
		}
		state.CachedUIDs = append(state.CachedUIDs, uid)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return state, err
	}
	return state, tx.Commit()
}

type record struct {
	summary mail.MessageSummary
	headers map[string][]string
}

// Recent is session state, not a persistent flag tracked by MODSEQ.
// See docs/concepts/cache-coherence.md for legacy summary cleanup.
func persistentFlags(flags []string) []string {
	if !hasFlag(flags, "\\Recent") {
		return flags
	}
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if !strings.EqualFold(flag, "\\Recent") {
			result = append(result, flag)
		}
	}
	return result
}

func readRecords(ctx context.Context, tx *sql.Tx, key mail.MailboxKey, includeHeaders bool) (map[uint32]record, error) {
	columns := "uid, summary"
	if includeHeaders {
		columns += ", headers"
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM messages WHERE account=? AND mailbox=?`, key.Account, key.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make(map[uint32]record)
	for rows.Next() {
		var uid uint32
		var summary, headers []byte
		dest := []any{&uid, &summary}
		if includeHeaders {
			dest = append(dest, &headers)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		var item record
		if err := json.Unmarshal(summary, &item.summary); err != nil {
			return nil, err
		}
		if includeHeaders {
			if err := json.Unmarshal(headers, &item.headers); err != nil {
				return nil, err
			}
		}
		records[uid] = item
	}
	return records, rows.Err()
}

// Apply commits a complete UID snapshot and its changes atomically. Membership
// reconciliation deletes only SQLite rows; it performs no remote operations.
func (c *Cache) Apply(ctx context.Context, key mail.MailboxKey, refresh mail.Refresh) error {
	diagnostics.Stage(ctx, "Updating header cache")
	defer diagnostics.Measure(ctx, "cache apply")()
	key, err := canonicalKey(key)
	if err != nil {
		return err
	}
	if refresh.UIDValidity == 0 {
		return mail.NewError("invalid_argument", "refresh UIDVALIDITY must be nonzero")
	}
	members := make(map[uint32]bool, len(refresh.Members))
	for _, uid := range refresh.Members {
		if uid == 0 || members[uid] {
			return mail.NewError("invalid_argument", "refresh membership contains a zero or duplicate UID")
		}
		members[uid] = true
	}
	metadata := make(map[uint32]record, len(refresh.NewMetadata))
	for _, item := range refresh.NewMetadata {
		if !members[item.UID] {
			return mail.NewError("invalid_argument", "metadata UID is absent from refresh membership")
		}
		if _, exists := metadata[item.UID]; exists {
			return mail.NewError("invalid_argument", "refresh contains duplicate metadata")
		}
		parsed, err := parseMetadata(mail.MessageRef{Mailbox: key.Name, UIDValidity: refresh.UIDValidity, UID: item.UID}, item)
		if err != nil {
			return err
		}
		metadata[item.UID] = parsed
	}
	flags := make(map[uint32]mail.FlagUpdate, len(refresh.FlagUpdates))
	for _, item := range refresh.FlagUpdates {
		if _, exists := flags[item.UID]; exists || !members[item.UID] {
			return mail.NewError("invalid_argument", "refresh flags contain duplicate or nonmember UIDs")
		}
		for _, flag := range item.Flags {
			if flag == "" || strings.ContainsAny(flag, " \t\r\n\x00") {
				return mail.NewError("invalid_argument", "refresh contains an invalid flag")
			}
		}
		flags[item.UID] = item
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := mailboxState(ctx, tx, key)
	if err != nil {
		return err
	}
	if state.Present && state.UIDValidity == refresh.UIDValidity && refresh.HighestModSeq != 0 && refresh.HighestModSeq < state.HighestModSeq {
		return mail.NewError("invalid_argument", "refresh MODSEQ checkpoint moved backwards")
	}
	old := make(map[uint32]record)
	if state.Present && state.UIDValidity == refresh.UIDValidity {
		old, err = readRecords(ctx, tx, key, false)
		if err != nil {
			return err
		}
	}
	for uid := range members {
		_, cached := old[uid]
		_, supplied := metadata[uid]
		_, hasFlags := flags[uid]
		if !cached && (!supplied || !hasFlags) {
			return mail.NewError("incomplete_refresh", "every uncached UID requires metadata and flags")
		}
		// Per-message MODSEQ may exceed the safe start-of-refresh checkpoint.
		if cached && hasFlags && state.HighestModSeq != 0 && flags[uid].ModSeq != 0 && flags[uid].ModSeq < old[uid].summary.ModSeq {
			return mail.NewError("invalid_argument", "message MODSEQ moved backwards within the cached epoch")
		}
	}
	if state.Present && state.UIDValidity != refresh.UIDValidity {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE account=? AND mailbox=?`, key.Account, key.Name); err != nil {
			return err
		}
	}
	// Unsigned 64-bit MODSEQ values are decimal TEXT, not signed SQLite INTEGERs.
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailboxes(account,name,uid_validity,highest_mod_seq) VALUES(?,?,?,?) ON CONFLICT(account,name) DO UPDATE SET uid_validity=excluded.uid_validity, highest_mod_seq=excluded.highest_mod_seq`, key.Account, key.Name, refresh.UIDValidity, strconv.FormatUint(refresh.HighestModSeq, 10)); err != nil {
		return err
	}
	for uid := range old {
		if !members[uid] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE account=? AND mailbox=? AND uid=?`, key.Account, key.Name, uid); err != nil {
				return err
			}
		}
	}
	for uid := range members {
		item, hasMetadata := metadata[uid]
		update, hasFlags := flags[uid]
		if !hasMetadata && !hasFlags && !hasFlag(old[uid].summary.Flags, "\\Recent") {
			continue
		}
		if !hasMetadata {
			item = old[uid]
		} else if previous, exists := old[uid]; exists {
			item.summary.Flags = previous.summary.Flags
			item.summary.ModSeq = previous.summary.ModSeq
		}
		if hasFlags {
			item.summary.Flags = append([]string{}, update.Flags...)
			item.summary.ModSeq = update.ModSeq
		}
		item.summary.Flags = persistentFlags(item.summary.Flags)
		summary, err := json.Marshal(item.summary)
		if err != nil {
			return err
		}
		if !hasMetadata {
			_, err = tx.ExecContext(ctx, `UPDATE messages SET summary=? WHERE account=? AND mailbox=? AND uid=?`, summary, key.Account, key.Name, uid)
		} else {
			var headers []byte
			headers, err = json.Marshal(item.headers)
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO messages(account,mailbox,uid,summary,headers) VALUES(?,?,?,?,?) ON CONFLICT(account,mailbox,uid) DO UPDATE SET summary=excluded.summary, headers=excluded.headers`, key.Account, key.Name, uid, summary, headers)
			}
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Invalidate resets the durable refresh checkpoint while retaining headers and
// UIDVALIDITY. The next refresh must fetch flags without a MODSEQ shortcut.
func (c *Cache) Invalidate(ctx context.Context, keys []mail.MailboxKey) error {
	normalized := make([]mail.MailboxKey, len(keys))
	for i, key := range keys {
		var err error
		normalized[i], err = canonicalKey(key)
		if err != nil {
			return err
		}
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range normalized {
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET highest_mod_seq='0' WHERE account=? AND name=?`, key.Account, key.Name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
