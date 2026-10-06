package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"slices"
	"strings"
)

// addressBookCap bounds the harvested address book. Once exceeded, the
// lowest-ranked entries are evicted so the book stays a useful set of the
// addresses the user actually corresponds with.
const addressBookCap = 1000

// addressRank orders the book for suggestions, listing and eviction alike: the
// addresses written to most come first and go last, then the most recent.
const addressRank = `ORDER BY sent_count DESC, last_used DESC`

// bulkLocalParts are the local parts of automated senders that are never
// worth suggesting as a recipient. Kept short on purpose: each one is a
// mailbox no person reads, not a guess at what looks automated.
var bulkLocalParts = []string{"noreply", "no-reply", "donotreply", "do-not-reply", "mailer-daemon", "postmaster"}

// AddressBookEntry is one harvested contact used for compose autocomplete.
// SentCount is how many messages the user sent to the address; UseCount also
// counts the mail received from it.
type AddressBookEntry struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	UseCount  int    `json:"useCount"`
	SentCount int    `json:"sentCount"`
	LastUsed  string `json:"lastUsed"`
	CreatedAt string `json:"createdAt"`
}

// AddressFilter decides which addresses only ever received from are learned
// and suggested. Addresses the user has sent to always pass. The zero value
// admits none of the others.
type AddressFilter struct {
	// Received admits every received-only address except bulk senders: those
	// with an automated local part such as noreply, and, when harvesting,
	// mail carrying a List-Unsubscribe header.
	Received bool
	// Trusted admits these bare addresses, bulk or not.
	Trusted []string
}

// admits reports whether a received-only address passes the filter. listed is
// whether the mail it came from carried a List-Unsubscribe header.
func (f AddressFilter) admits(email string, listed bool) bool {
	if slices.Contains(f.Trusted, email) {
		return true
	}
	return f.Received && !listed && !isBulkAddress(email)
}

// isBulkAddress reports whether a bare, lowercased address has one of the
// automated local parts.
func isBulkAddress(email string) bool {
	local, _, _ := strings.Cut(email, "@")
	return slices.Contains(bulkLocalParts, local)
}

// RecordAddress learns an address the user sent to. It upserts the entry,
// bumping sent_count, use_count and last_used, filling in a name only when one
// is known, then prunes the book back under its cap. A blank email is ignored.
func (d *DB) RecordAddress(ctx context.Context, email, name string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil
	}
	name = strings.TrimSpace(name)
	now := nowText()
	const query = `
INSERT INTO address_book (email, name, use_count, sent_count, last_used, created_at)
VALUES (?, ?, 1, 1, ?, ?)
ON CONFLICT(email) DO UPDATE SET
    use_count = use_count + 1,
    sent_count = sent_count + 1,
    last_used = excluded.last_used,
    -- keep an existing name unless we now have one and had none before.
    name = CASE WHEN address_book.name = '' THEN excluded.name ELSE address_book.name END`
	if _, err := d.sql.ExecContext(ctx, query, email, name, now, now); err != nil {
		return fmt.Errorf("storage: record address %q: %w", email, err)
	}
	return d.pruneAddressBook(ctx)
}

// harvestedAddress is one bare address gathered from cached mail.
type harvestedAddress struct {
	name     string
	count    int
	lastUsed string
}

// HarvestSenders seeds the address book from the senders already cached in the
// messages table that filter admits. Each address in a stored From list is
// keyed by its bare, lowercased form. It only inserts addresses not present
// yet (INSERT OR IGNORE), so re-running is idempotent and never clobbers
// counts built up from sends. Entries keyed "name <addr>" by older harvests
// are folded into their bare address first, and ones no address can be read
// from are dropped.
func (d *DB) HarvestSenders(ctx context.Context, filter AddressFilter) error {
	senders, order, err := d.cachedSenders(ctx, filter)
	if err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin harvest senders: %w", err)
	}
	defer tx.Rollback()

	// the repair runs first so a folded entry is not counted again by the
	// insert below, which comes from the same messages.
	if err := repairAddressBookKeys(ctx, tx); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT OR IGNORE INTO address_book (email, name, use_count, last_used, created_at)
VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("storage: prepare harvest senders: %w", err)
	}
	defer stmt.Close()
	named, err := tx.PrepareContext(ctx, `UPDATE address_book SET name = ? WHERE email = ? AND name = ''`)
	if err != nil {
		return fmt.Errorf("storage: prepare harvest sender names: %w", err)
	}
	defer named.Close()
	now := nowText()
	for _, email := range order {
		s := senders[email]
		if _, err := stmt.ExecContext(ctx, email, s.name, s.count, s.lastUsed, now); err != nil {
			return fmt.Errorf("storage: harvest sender %q: %w", email, err)
		}
		// an entry that exists without a name (a repaired key, or one made by
		// sending) takes the name cached mail gives the address.
		if s.name != "" {
			if _, err := named.ExecContext(ctx, s.name, email); err != nil {
				return fmt.Errorf("storage: name harvested sender %q: %w", email, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit harvest senders: %w", err)
	}
	return d.pruneAddressBook(ctx)
}

// cachedSenders aggregates the cached From lists that filter admits per bare,
// lowercased address, returning the addresses in first-seen order so inserts
// are deterministic.
func (d *DB) cachedSenders(ctx context.Context, filter AddressFilter) (map[string]*harvestedAddress, []string, error) {
	const query = `
SELECT from_address, MAX(from_name), COUNT(*), MAX(date), list_unsubscribe != '' AS listed
FROM messages
WHERE from_address LIKE '%@%'
GROUP BY from_address, listed`
	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read cached senders: %w", err)
	}
	defer rows.Close()

	senders := make(map[string]*harvestedAddress)
	var order []string
	for rows.Next() {
		var from, fromName, lastUsed string
		var count int
		var listed bool
		if err := rows.Scan(&from, &fromName, &count, &lastUsed, &listed); err != nil {
			return nil, nil, fmt.Errorf("storage: scan cached sender: %w", err)
		}
		addrs := parseAddressList(from)
		for _, a := range addrs {
			if !filter.admits(a.Address, listed) {
				continue
			}
			name := a.Name
			if name == "" && len(addrs) == 1 {
				name = fromName
			}
			s, ok := senders[a.Address]
			if !ok {
				s = &harvestedAddress{}
				senders[a.Address] = s
				order = append(order, a.Address)
			}
			s.count += count
			s.lastUsed = max(s.lastUsed, lastUsed)
			if s.name == "" {
				s.name = name
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("storage: iterate cached senders: %w", err)
	}
	return senders, order, nil
}

// BackfillSentCounts sets each address's sent_count from the cached messages
// in the given folders (the Sent folders): the number of those messages that
// list it in To or Cc. A stored count is only ever raised, never lowered or
// added to, so re-running counts nothing twice and keeps sends whose Sent copy
// is not cached. Recipients missing from the book are added.
func (d *DB) BackfillSentCounts(ctx context.Context, folderIDs []int64) error {
	if len(folderIDs) == 0 {
		return nil
	}
	recipients, order, err := d.sentRecipients(ctx, folderIDs)
	if err != nil {
		return err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin sent backfill: %w", err)
	}
	defer tx.Rollback()
	if err := repairAddressBookKeys(ctx, tx); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO address_book (email, name, use_count, sent_count, last_used, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(email) DO UPDATE SET
    sent_count = MAX(address_book.sent_count, excluded.sent_count),
    use_count = MAX(address_book.use_count, excluded.sent_count),
    last_used = MAX(address_book.last_used, excluded.last_used),
    name = CASE WHEN address_book.name = '' THEN excluded.name ELSE address_book.name END`)
	if err != nil {
		return fmt.Errorf("storage: prepare sent backfill: %w", err)
	}
	defer stmt.Close()
	now := nowText()
	for _, email := range order {
		r := recipients[email]
		if _, err := stmt.ExecContext(ctx, email, r.name, r.count, r.count, r.lastUsed, now); err != nil {
			return fmt.Errorf("storage: backfill sent count %q: %w", email, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit sent backfill: %w", err)
	}
	return d.pruneAddressBook(ctx)
}

// sentRecipients counts, per bare lowercased address, the messages in folderIDs
// that list it in To or Cc, returning the addresses in first-seen order.
func (d *DB) sentRecipients(ctx context.Context, folderIDs []int64) (map[string]*harvestedAddress, []string, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(folderIDs)), ",")
	args := make([]any, 0, len(folderIDs))
	for _, id := range folderIDs {
		args = append(args, id)
	}
	rows, err := d.sql.QueryContext(ctx, `
SELECT to_addresses, cc_addresses, date FROM messages
WHERE folder_id IN (`+marks+`) AND (to_addresses != '' OR cc_addresses != '')`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: read sent recipients: %w", err)
	}
	defer rows.Close()

	recipients := make(map[string]*harvestedAddress)
	var order []string
	for rows.Next() {
		var to, cc, date string
		if err := rows.Scan(&to, &cc, &date); err != nil {
			return nil, nil, fmt.Errorf("storage: scan sent recipients: %w", err)
		}
		seen := make(map[string]bool)
		for _, list := range []string{to, cc} {
			if list == "" {
				continue
			}
			for _, a := range parseAddressList(list) {
				if seen[a.Address] {
					continue
				}
				seen[a.Address] = true
				r, ok := recipients[a.Address]
				if !ok {
					r = &harvestedAddress{}
					recipients[a.Address] = r
					order = append(order, a.Address)
				}
				r.count++
				r.lastUsed = max(r.lastUsed, date)
				if r.name == "" {
					r.name = a.Name
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("storage: iterate sent recipients: %w", err)
	}
	return recipients, order, nil
}

// repairAddressBookKeys folds entries whose key is a "name <addr>" list into
// the bare address of each address in it: the counts are added, the later
// last_used and a non-empty stored name are kept. An entry no address can be
// read from is deleted.
func repairAddressBookKeys(ctx context.Context, tx *sql.Tx) error {
	type badEntry struct {
		email, name, lastUsed, createdAt string
		count, sent                      int
	}
	rows, err := tx.QueryContext(ctx, `
SELECT email, name, use_count, sent_count, last_used, created_at FROM address_book
WHERE instr(email, '<') > 0 OR instr(email, ',') > 0`)
	if err != nil {
		return fmt.Errorf("storage: read malformed address keys: %w", err)
	}
	// read every row before writing: SQLite leaves it undefined whether a
	// cursor sees rows inserted into the table it is scanning.
	var bad []badEntry
	for rows.Next() {
		var e badEntry
		if err := rows.Scan(&e.email, &e.name, &e.count, &e.sent, &e.lastUsed, &e.createdAt); err != nil {
			rows.Close()
			return fmt.Errorf("storage: scan malformed address key: %w", err)
		}
		bad = append(bad, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("storage: iterate malformed address keys: %w", err)
	}

	for _, e := range bad {
		addrs := parseAddressList(e.email)
		for _, a := range addrs {
			// the key was stored lowercased, so its display name is no
			// source for one; the harvest below names it from cached mail.
			name := e.name
			if len(addrs) > 1 {
				name = ""
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO address_book (email, name, use_count, sent_count, last_used, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(email) DO UPDATE SET
    use_count = address_book.use_count + excluded.use_count,
    sent_count = address_book.sent_count + excluded.sent_count,
    last_used = MAX(address_book.last_used, excluded.last_used),
    name = CASE WHEN address_book.name = '' THEN excluded.name ELSE address_book.name END`,
				a.Address, name, e.count, e.sent, e.lastUsed, e.createdAt); err != nil {
				return fmt.Errorf("storage: fold address key %q: %w", e.email, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM address_book WHERE email = ?`, e.email); err != nil {
			return fmt.Errorf("storage: delete address key %q: %w", e.email, err)
		}
	}
	return nil
}

// parseAddressList reads a stored address list into addresses with bare,
// lowercased Address fields. A list net/mail rejects falls back to the text
// inside its last angle brackets, or to the whole text when it is one bare
// address; anything else yields nil.
func parseAddressList(list string) []*mail.Address {
	addrs, err := mail.ParseAddressList(list)
	if err != nil {
		bare := strings.TrimSpace(list)
		if open := strings.LastIndex(bare, "<"); open >= 0 {
			end := strings.Index(bare[open:], ">")
			if end < 0 {
				return nil
			}
			bare = bare[open+1 : open+end]
		}
		bare = strings.TrimSpace(bare)
		if !strings.Contains(bare, "@") || strings.ContainsAny(bare, " ,<>\"") {
			return nil
		}
		addrs = []*mail.Address{{Address: bare}}
	}
	for _, a := range addrs {
		a.Address = strings.ToLower(a.Address)
	}
	return addrs
}

// pruneAddressBook evicts the lowest-ranked entries beyond the cap.
func (d *DB) pruneAddressBook(ctx context.Context) error {
	query := `
DELETE FROM address_book WHERE email IN (
    SELECT email FROM address_book ` + addressRank + `
    LIMIT -1 OFFSET ?
)`
	if _, err := d.sql.ExecContext(ctx, query, addressBookCap); err != nil {
		return fmt.Errorf("storage: prune address book: %w", err)
	}
	return nil
}

// SearchAddresses returns autocomplete candidates matching q (against email or
// name), ranked. Addresses the user has sent to are always candidates; filter
// decides which received-only ones join them. An empty q returns the top
// entries.
func (d *DB) SearchAddresses(ctx context.Context, q string, limit int, filter AddressFilter) ([]AddressBookEntry, error) {
	q = strings.TrimSpace(strings.ToLower(q))
	like := "%" + escapeLike(q) + "%"
	args := []any{q, like, like}
	admitted := []string{"sent_count > 0"}
	if len(filter.Trusted) > 0 {
		admitted = append(admitted, "email IN ("+strings.TrimSuffix(strings.Repeat("?,", len(filter.Trusted)), ",")+")")
		for _, t := range filter.Trusted {
			args = append(args, strings.ToLower(t))
		}
	}
	if filter.Received {
		admitted = append(admitted, "substr(email, 1, instr(email, '@') - 1) NOT IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(bulkLocalParts)), ",")+")")
		for _, local := range bulkLocalParts {
			args = append(args, local)
		}
	}
	args = append(args, normalizeLimit(limit))
	query := `
SELECT email, name, use_count, sent_count, last_used, created_at
FROM address_book
WHERE (? = '' OR lower(email) LIKE ? ESCAPE '\' OR lower(name) LIKE ? ESCAPE '\')
  AND (` + strings.Join(admitted, " OR ") + `)
` + addressRank + `
LIMIT ?`
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: search addresses: %w", err)
	}
	defer rows.Close()
	return scanAddressBook(rows)
}

// ListAddresses returns the whole book for the settings manager, ranked,
// whatever the learning level: a row filtered out of suggestions is still the
// user's to review and remove.
func (d *DB) ListAddresses(ctx context.Context) ([]AddressBookEntry, error) {
	rows, err := d.sql.QueryContext(ctx, `
SELECT email, name, use_count, sent_count, last_used, created_at
FROM address_book `+addressRank)
	if err != nil {
		return nil, fmt.Errorf("storage: list addresses: %w", err)
	}
	defer rows.Close()
	return scanAddressBook(rows)
}

// DeleteAddress removes one harvested contact.
func (d *DB) DeleteAddress(ctx context.Context, email string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if _, err := d.sql.ExecContext(ctx, `DELETE FROM address_book WHERE email = ?`, email); err != nil {
		return fmt.Errorf("storage: delete address %q: %w", email, err)
	}
	return nil
}

func scanAddressBook(rows *sql.Rows) ([]AddressBookEntry, error) {
	var out []AddressBookEntry
	for rows.Next() {
		var e AddressBookEntry
		if err := rows.Scan(&e.Email, &e.Name, &e.UseCount, &e.SentCount, &e.LastUsed, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage: scan address: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate addresses: %w", err)
	}
	return out, nil
}

// escapeLike escapes the LIKE wildcards in user input so a literal % or _ typed
// in the search box does not act as a wildcard.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
