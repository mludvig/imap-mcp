package imapclient

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// UnilateralDataVanished describes removed message UIDs.
type UnilateralDataVanished struct {
	UIDs imap.UIDSet
	// Earlier reports historical removals and does not change the current count.
	Earlier bool
}

func (c *Client) handleVanished() error {
	// VANISHED support alone is not QRESYNC; see docs/adr/0004-backport-imap4rev2-vanished.md.
	dec := c.dec
	if !dec.ExpectSpecial(' ') {
		return dec.Err()
	}
	var data UnilateralDataVanished
	if dec.Special('(') {
		var modifier string
		if !dec.ExpectAtom(&modifier) || !dec.Expect(strings.EqualFold(modifier, "EARLIER"), "EARLIER") ||
			!dec.ExpectSpecial(')') || !dec.ExpectSpecial(' ') {
			return dec.Err()
		}
		data.Earlier = true
	}
	if !dec.ExpectUIDSet(&data.UIDs) || !dec.Expect(!data.UIDs.Dynamic(), "static UID set") {
		return dec.Err()
	}
	// The decoder normalizes ranges. Count without expanding them: uint32
	// enumeration can wrap at the maximum UID, and large sets must stay bounded.
	var count uint64
	for _, r := range data.UIDs {
		if !dec.Expect(r.Start > 0 && r.Stop >= r.Start, "non-zero UID range") {
			return dec.Err()
		}
		count += uint64(r.Stop) - uint64(r.Start) + 1
	}
	if !data.Earlier {
		c.mutex.Lock()
		if c.state == imap.ConnStateSelected {
			c.mailbox = c.mailbox.copy()
			if count >= uint64(c.mailbox.NumMessages) {
				c.mailbox.NumMessages = 0
			} else {
				c.mailbox.NumMessages -= uint32(count)
			}
		}
		c.mutex.Unlock()
	}
	if handler := c.options.unilateralDataHandler().Vanished; handler != nil {
		handler(&data)
	}
	return nil
}
