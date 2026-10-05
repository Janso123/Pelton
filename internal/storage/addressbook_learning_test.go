package storage

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestRecordAddressCountsSends(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	for range 2 {
		if err := db.RecordAddress(ctx, "Ann@X.org", "Ann"); err != nil {
			t.Fatal(err)
		}
	}
	ann := addressBookByEmail(t, db)["ann@x.org"]
	if ann.SentCount != 2 || ann.UseCount != 2 {
		t.Errorf("ann %+v, want sent count 2 and use count 2", ann)
	}
}

// insertAddressRow writes an address book row directly, for ranking tests that
// need counts no sequence of sends would produce cheaply.
func insertAddressRow(t *testing.T, db *DB, email string, useCount, sentCount int, lastUsed string) {
	t.Helper()
	if _, err := db.sql.ExecContext(context.Background(), `
INSERT INTO address_book (email, name, use_count, sent_count, last_used, created_at)
VALUES (?, '', ?, ?, ?, ?)`, email, useCount, sentCount, lastUsed, lastUsed); err != nil {
		t.Fatal(err)
	}
}

func entryEmails(entries []AddressBookEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Email)
	}
	return out
}

// Someone the user writes to outranks a sender heard from far more often, and
// among equals the more recent one comes first.
func TestAddressRankingPutsSentFirst(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	insertAddressRow(t, db, "news@shop.com", 90, 0, "2026-09-01T00:00:00Z")
	insertAddressRow(t, db, "old@friend.org", 1, 1, "2026-01-01T00:00:00Z")
	insertAddressRow(t, db, "boss@work.com", 3, 3, "2026-02-01T00:00:00Z")
	insertAddressRow(t, db, "new@friend.org", 1, 1, "2026-03-01T00:00:00Z")
	want := []string{"boss@work.com", "new@friend.org", "old@friend.org", "news@shop.com"}

	listed, err := db.ListAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryEmails(listed); !slices.Equal(got, want) {
		t.Errorf("list order %q, want %q", got, want)
	}
	found, err := db.SearchAddresses(ctx, "", 10, AddressFilter{Received: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryEmails(found); !slices.Equal(got, want) {
		t.Errorf("search order %q, want %q", got, want)
	}
}

// A full book evicts received-only senders before anyone the user wrote to,
// however often those senders wrote.
func TestPruneKeepsSentAddresses(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range addressBookCap {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO address_book (email, name, use_count, sent_count, last_used, created_at)
VALUES (?, '', 50, 0, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`, fmt.Sprintf("bulk%d@shop.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAddress(ctx, "friend@x.org", ""); err != nil {
		t.Fatal(err)
	}
	book := addressBookByEmail(t, db)
	if len(book) != addressBookCap {
		t.Errorf("book holds %d, want the cap %d", len(book), addressBookCap)
	}
	if _, ok := book["friend@x.org"]; !ok {
		t.Error("the address written to was pruned")
	}
}

// addressTestFolder creates a folder on the test account, creating the account
// on first use.
func addressTestFolder(t *testing.T, db *DB, path string) Folder {
	t.Helper()
	ctx := context.Background()
	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var accountID int64
	if len(accounts) > 0 {
		accountID = accounts[0].ID
	} else if accountID, err = db.CreateAccount(ctx, &Account{Email: "me@example.com"}); err != nil {
		t.Fatal(err)
	}
	folder := Folder{AccountID: accountID, Name: path, IMAPPath: path, RemoteID: path}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// The backfill counts the messages in Sent folders addressing each recipient
// once per message, and taking the larger of that and the stored count lets it
// run again without counting anything twice.
func TestBackfillSentCountsFromSentFolders(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sent := addressTestFolder(t, db, "Sent")
	inbox := addressTestFolder(t, db, "INBOX")
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	msgs := []struct {
		folder Folder
		to, cc string
	}{
		{sent, "Ann <Ann@X.org>, bob@y.org", "ann@x.org"},
		{sent, "ann@x.org", ""},
		{sent, "", "Carl Cc <carl@z.org>"},
		{inbox, "ann@x.org, me@example.com", "dora@w.org"},
	}
	for i, m := range msgs {
		if _, err := db.InsertMessage(ctx, &Message{AccountID: m.folder.AccountID, FolderID: m.folder.ID,
			UID: uint32(i + 1), FromAddress: "me@example.com", ToAddresses: m.to, CcAddresses: m.cc,
			Date: day.AddDate(0, 0, i), Subject: "s", BodyComplete: true}); err != nil {
			t.Fatal(err)
		}
	}
	// a send recorded before the backfill already counts one of ann's messages.
	if err := db.RecordAddress(ctx, "ann@x.org", ""); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := db.BackfillSentCounts(ctx, []int64{sent.ID}); err != nil {
			t.Fatal(err)
		}
		book := addressBookByEmail(t, db)
		want := map[string]struct {
			name string
			sent int
		}{
			"ann@x.org":  {"Ann", 2},
			"bob@y.org":  {"", 1},
			"carl@z.org": {"Carl Cc", 1},
		}
		if len(book) != len(want) {
			t.Fatalf("book %v, want only the Sent recipients", book)
		}
		for email, w := range want {
			e := book[email]
			if e.SentCount != w.sent || e.Name != w.name {
				t.Errorf("%s: %+v, want name %q sent count %d", email, e, w.name, w.sent)
			}
		}
		if carl := book["carl@z.org"]; carl.LastUsed != formatTime(day.AddDate(0, 0, 2)) {
			t.Errorf("carl last used %q, want the date of the message sent to him", carl.LastUsed)
		}
	}
}

// Suggestions keep the people written to at every level; the filter decides
// which received-only senders join them.
func TestSearchAddressesFilter(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	const when = "2026-05-01T00:00:00Z"
	insertAddressRow(t, db, "friend@x.org", 1, 1, when)
	insertAddressRow(t, db, "noreply@x.org", 1, 1, when)
	insertAddressRow(t, db, "trusted@x.org", 5, 0, when)
	insertAddressRow(t, db, "stranger@x.org", 4, 0, when)
	insertAddressRow(t, db, "no-reply@x.org", 3, 0, when)
	insertAddressRow(t, db, "mailer-daemon@x.org", 2, 0, when)

	tests := []struct {
		name   string
		filter AddressFilter
		want   []string
	}{
		{"sent only", AddressFilter{}, []string{"friend@x.org", "noreply@x.org"}},
		{"trusted", AddressFilter{Trusted: []string{"trusted@x.org"}},
			[]string{"friend@x.org", "noreply@x.org", "trusted@x.org"}},
		{"received", AddressFilter{Received: true},
			[]string{"friend@x.org", "noreply@x.org", "trusted@x.org", "stranger@x.org"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.SearchAddresses(ctx, "x.org", 10, tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(entryEmails(got), tt.want) {
				t.Errorf("got %q, want %q", entryEmails(got), tt.want)
			}
		})
	}
}

// Mailing lists and automated senders are not people to suggest, so the
// harvest leaves them out.
func TestHarvestSendersSkipsBulkSenders(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	inbox := addressTestFolder(t, db, "INBOX")
	senders := []struct{ from, unsubscribe string }{
		{"Person <person@x.org>", ""},
		{"Shop <news@shop.com>", "<mailto:leave@shop.com>"},
		{"NoReply@Bank.com", ""},
		{"do-not-reply@x.org", ""},
		{"MAILER-DAEMON@mx.org", ""},
		{"postmaster@mx.org", ""},
	}
	for i, s := range senders {
		if _, err := db.InsertMessage(ctx, &Message{AccountID: inbox.AccountID, FolderID: inbox.ID,
			UID: uint32(i + 1), FromAddress: s.from, ListUnsubscribe: s.unsubscribe,
			Subject: "s", BodyComplete: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.HarvestSenders(ctx, AddressFilter{Received: true}); err != nil {
		t.Fatal(err)
	}
	book := addressBookByEmail(t, db)
	if _, ok := book["person@x.org"]; len(book) != 1 || !ok {
		t.Errorf("book %v, want only person@x.org", book)
	}

	// a trusted sender is learned even when it is a mailing list, and only
	// trusted senders are learned when received mail at large is not.
	if err := db.HarvestSenders(ctx, AddressFilter{Trusted: []string{"news@shop.com"}}); err != nil {
		t.Fatal(err)
	}
	book = addressBookByEmail(t, db)
	if _, ok := book["news@shop.com"]; len(book) != 2 || !ok {
		t.Errorf("book %v, want person@x.org and the trusted news@shop.com", book)
	}
}
