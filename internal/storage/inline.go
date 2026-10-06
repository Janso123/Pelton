package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// normalizeContentID is how a content id is compared: without angle brackets
// and lowercased, the way cid: urls in the html are matched.
func normalizeContentID(id string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(id), "<>"))
}

// MarkMissingInlineParts flags every complete cached message whose html points
// at a cid: picture it has no attachment for, and returns how many it found.
// The parser used to drop pictures sent with Content-Disposition: inline (what
// Outlook does), and the raw source is not kept, so a sync of their folder
// fetches the missing parts again. Local Folders mail has no server to fetch
// from and is skipped. referenced returns the lowercased content ids an html
// body refers to.
//
// It walks every message with a cid: reference, which is why it is called once,
// in the background, rather than on every start.
func (d *DB) MarkMissingInlineParts(ctx context.Context, referenced func(html string) map[string]bool) (int, error) {
	stored, err := d.contentIDsByMessage(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := d.sql.QueryContext(ctx, `
SELECT id, body_html FROM messages
 WHERE needs_refetch = 0 AND body_complete = 1 AND body_html LIKE '%cid:%'
   AND account_id NOT IN (SELECT id FROM accounts WHERE is_local = 1)`)
	if err != nil {
		return 0, fmt.Errorf("storage: scan messages for missing inline parts: %w", err)
	}
	defer rows.Close()

	var batch []int64
	for rows.Next() {
		var (
			id       int64
			htmlBody string
		)
		if err := rows.Scan(&id, &htmlBody); err != nil {
			return 0, fmt.Errorf("storage: scan message html: %w", err)
		}
		for cid := range referenced(htmlBody) {
			if !stored[id][cid] {
				batch = append(batch, id)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("storage: iterate messages for missing inline parts: %w", err)
	}
	for start := 0; start < len(batch); start += markBatch {
		if err := d.markRefetch(ctx, batch[start:min(start+markBatch, len(batch))]); err != nil {
			return 0, err
		}
	}
	return len(batch), nil
}

// contentIDsByMessage returns the normalized content ids stored per message.
func (d *DB) contentIDsByMessage(ctx context.Context) (map[int64]map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT message_id, content_id FROM attachments WHERE content_id != ''`)
	if err != nil {
		return nil, fmt.Errorf("storage: list attachment content ids: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]map[string]bool)
	for rows.Next() {
		var (
			id  int64
			cid string
		)
		if err := rows.Scan(&id, &cid); err != nil {
			return nil, fmt.Errorf("storage: scan attachment content id: %w", err)
		}
		if out[id] == nil {
			out[id] = make(map[string]bool)
		}
		out[id][normalizeContentID(cid)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate attachment content ids: %w", err)
	}
	return out, nil
}

// AddMissingInlineAttachments stores the parts of atts that carry a content id
// the message has no attachment for, and returns how many it added. What is
// already stored stays as it is, so a refetch only fills the gaps. Parts without
// a content id are skipped: the message kept those all along.
func (d *DB) AddMissingInlineAttachments(ctx context.Context, messageID int64, atts []IncomingAttachment) (int, error) {
	var accountID int64
	if err := d.sql.QueryRowContext(ctx,
		`SELECT account_id FROM messages WHERE id = ?`, messageID).Scan(&accountID); err != nil {
		return 0, fmt.Errorf("storage: look up message %d: %w", messageID, err)
	}
	// the stored ids are read inside the transaction, which takes the write
	// lock at BEGIN (_txlock=immediate): a second fill of the same message
	// waits here and then sees what the first one added.
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("storage: begin inline attachment fill: %w", err)
	}
	defer tx.Rollback()

	have, err := storedContentIDs(ctx, tx, messageID)
	if err != nil {
		return 0, err
	}

	var written []string
	for _, in := range atts {
		cid := normalizeContentID(in.ContentID)
		if cid == "" || have[cid] {
			continue
		}
		saved, err := d.writeAttachmentFile(accountID, messageID, in.Filename, in.Content)
		if err != nil {
			d.removeAttachmentFiles(written)
			return 0, err
		}
		written = append(written, saved.DiskPath)
		saved.MessageID = messageID
		saved.ContentType = in.ContentType
		saved.ContentID = in.ContentID
		if err := insertAttachment(ctx, tx, saved); err != nil {
			d.removeAttachmentFiles(written)
			return 0, err
		}
		have[cid] = true
	}
	if len(written) == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET has_attachments = 1 WHERE id = ?`, messageID); err != nil {
		d.removeAttachmentFiles(written)
		return 0, fmt.Errorf("storage: flag attachments on message %d: %w", messageID, err)
	}
	if err := tx.Commit(); err != nil {
		d.removeAttachmentFiles(written)
		return 0, fmt.Errorf("storage: commit inline attachment fill: %w", err)
	}
	return len(written), nil
}

// storedContentIDs returns the normalized content ids a message already has.
func storedContentIDs(ctx context.Context, tx *sql.Tx, messageID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT content_id FROM attachments WHERE message_id = ? AND content_id != ''`, messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: list content ids of message %d: %w", messageID, err)
	}
	defer rows.Close()
	have := make(map[string]bool)
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, fmt.Errorf("storage: scan content id: %w", err)
		}
		have[normalizeContentID(cid)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate content ids: %w", err)
	}
	return have, nil
}
