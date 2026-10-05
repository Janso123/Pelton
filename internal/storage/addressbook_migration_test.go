package storage

import (
	"context"
	"strings"
	"testing"
)

// Before 0043 use_count summed the sends to an address and, for an entry the
// harvest created, the mail received from it. The migration estimates the sends
// as use_count minus the cached mail from that address, so the people the user
// wrote to are not hidden at the default level just because their Sent copy is
// not cached. Keys an old harvest stored as "name <addr>" came only from
// received mail and get none, and neither do bulk senders (a noreply local
// part, or cached mail with List-Unsubscribe): the harvest counted mail since
// deleted from the cache, so their estimate would be positive.
func TestMigration0043EstimatesSentCounts(t *testing.T) {
	ctx := context.Background()
	db := openDBThrough(t, 42)

	var accountID, folderID int64
	if err := db.sql.QueryRowContext(ctx,
		`INSERT INTO accounts (email, imap_host, imap_port, created_at) VALUES (?, ?, ?, ?) RETURNING id`,
		"me@example.com", "imap.example", 993, nowText()).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRowContext(ctx,
		`INSERT INTO folders (account_id, name, imap_path, remote_id) VALUES (?, ?, ?, ?) RETURNING id`,
		accountID, "INBOX", "INBOX", "INBOX").Scan(&folderID); err != nil {
		t.Fatal(err)
	}
	// news@x.org wrote 4 times and was never written to; ann@y.org wrote twice
	// and was written to 3 times; bob@z.org was only written to.
	from := []string{"News <news@x.org>", "news@x.org", "NEWS@x.org", "News <News@X.org>", "Ann <ann@y.org>", "ann@y.org"}
	// promo@shop.example and noreply@bank.example were harvested from mail
	// that has mostly been deleted since; one message each is still cached.
	from = append(from, "Shop <promo@shop.example>", "noreply@bank.example")
	for i, f := range from {
		unsubscribe := ""
		if f == "Shop <promo@shop.example>" {
			unsubscribe = "<mailto:leave@shop.example>"
		}
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO messages (account_id, folder_id, uid, remote_id, from_address, list_unsubscribe) VALUES (?, ?, ?, ?, ?, ?)`,
			accountID, folderID, i+1, string(rune('a'+i)), f, unsubscribe); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		email string
		count int
	}{{"news@x.org", 4}, {"ann@y.org", 5}, {"bob@z.org", 2}, {"ann <ann@y.org>", 2}, {"promo@shop.example", 9}, {"noreply@bank.example", 6}} {
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO address_book (email, name, use_count, last_used, created_at) VALUES (?, '', ?, ?, ?)`,
			r.email, r.count, nowText(), nowText()); err != nil {
			t.Fatal(err)
		}
	}

	if err := applyMigrationVersion(ctx, db, 43); err != nil {
		t.Fatalf("apply 0043: %v", err)
	}

	want := map[string]int{"news@x.org": 0, "ann@y.org": 3, "bob@z.org": 2, "ann <ann@y.org>": 0,
		"promo@shop.example": 0, "noreply@bank.example": 0}
	for email, w := range want {
		var got int
		if err := db.sql.QueryRowContext(ctx, `SELECT sent_count FROM address_book WHERE email = ?`, email).Scan(&got); err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		if got != w {
			t.Errorf("%s: sent_count %d, want %d", email, got, w)
		}
	}
}

// 0043 spells out the bulk local parts in SQL; it must skip every one the
// suggestion filter knows.
func TestMigration0043SkipsEveryBulkLocalPart(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/0043_address_book_sent_count.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range bulkLocalParts {
		if !strings.Contains(string(sql), "'"+local+"'") {
			t.Errorf("0043 does not skip %q", local)
		}
	}
}
