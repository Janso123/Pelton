package storage

import (
	"context"
	"slices"
	"testing"
	"time"
)

func addressBookByEmail(t *testing.T, db *DB) map[string]AddressBookEntry {
	t.Helper()
	entries, err := db.ListAddresses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]AddressBookEntry, len(entries))
	for _, e := range entries {
		out[e.Email] = e
	}
	return out
}

// messages.from_address holds the whole From list ("Name <addr>, ..."), so the
// harvest has to key the book by each bare, lowercased address in it.
func TestHarvestSendersKeysBareLowercaseAddresses(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	accountID, err := db.CreateAccount(ctx, &Account{Email: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	folder := Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX", RemoteID: "INBOX"}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	senders := []struct{ from, name string }{
		{"Jane Doe <Jane@X.org>", "Jane Doe"},
		{"Jane Doe <Jane@X.org>", "Jane Doe"},
		{"jane@x.org", ""},
		{`"Doe, John" <john@y.org>, bob@z.org`, "Doe, John"},
		{"carol@w.org", "Carol W"},
		{"dave@v.org", ""},
		{"Erin <erin@u.org>", ""},
	}
	for i, s := range senders {
		if _, err := db.UpsertMessageListMeta(ctx, &Message{
			AccountID: accountID, FolderID: folder.ID, RemoteID: string(rune('a' + i)),
			FromAddress: s.from, FromName: s.name, Date: day.AddDate(0, 0, i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// a count built up from sends must survive the harvest, and the entry
	// sending left without a name is named from cached mail.
	for range 5 {
		if err := db.RecordAddress(ctx, "erin@u.org", ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.HarvestSenders(ctx, AddressFilter{Received: true}); err != nil {
		t.Fatal(err)
	}

	got := addressBookByEmail(t, db)
	want := map[string]struct {
		name  string
		count int
	}{
		"jane@x.org":  {"Jane Doe", 3},
		"john@y.org":  {"Doe, John", 1},
		"bob@z.org":   {"", 1},
		"carol@w.org": {"Carol W", 1},
		"dave@v.org":  {"", 1},
		"erin@u.org":  {"Erin", 5}, // a nameless entry from sending takes the name cached mail gives
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(got) != len(want) {
		t.Fatalf("book keys %q, want %d bare addresses", keys, len(want))
	}
	for email, w := range want {
		e, ok := got[email]
		if !ok {
			t.Errorf("missing %q in %q", email, keys)
			continue
		}
		if e.Name != w.name || e.UseCount != w.count {
			t.Errorf("%s: name %q count %d, want %q %d", email, e.Name, e.UseCount, w.name, w.count)
		}
	}
	if jane := got["jane@x.org"]; jane.LastUsed != formatTime(day.AddDate(0, 0, 2)) {
		t.Errorf("jane last used %q, want the latest message date", jane.LastUsed)
	}
}

// Books harvested before the fix hold "name <addr>" keys. The harvest folds each
// into its bare address, adding its count to an existing row, and drops a key
// no address can be read from.
func TestHarvestSendersRepairsNameAddrKeys(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	const early, late = "2026-01-02T00:00:00Z", "2026-03-04T00:00:00Z"
	rows := []struct {
		email, name string
		count       int
		last        string
	}{
		{"jane doe <jane@x.org>", "Jane Doe", 4, late},
		{"jane@x.org", "", 2, early},
		{`"doe, john" <john@y.org>`, "", 3, early},
		{"kim lee <kim@z.org>", "", 1, early},
		{"garbage <nope", "", 7, early},
	}
	for _, r := range rows {
		if _, err := db.sql.ExecContext(ctx,
			`INSERT INTO address_book (email, name, use_count, last_used, created_at) VALUES (?, ?, ?, ?, ?)`,
			r.email, r.name, r.count, r.last, early); err != nil {
			t.Fatal(err)
		}
	}

	// kim's cached mail still carries the name in its original case.
	accountID, err := db.CreateAccount(ctx, &Account{Email: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	folder := Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, &Message{AccountID: accountID, FolderID: folder.ID, UID: 1,
		FromAddress: "Kim Lee <Kim@Z.org>", FromName: "Kim Lee", Subject: "hi", BodyComplete: true}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := db.HarvestSenders(ctx, AddressFilter{Received: true}); err != nil {
			t.Fatal(err)
		}
		got := addressBookByEmail(t, db)
		if len(got) != 3 {
			t.Fatalf("book %v, want only jane@x.org, john@y.org and kim@z.org", got)
		}
		// the old key was stored lowercased, so it is no source for a name.
		if kim := got["kim@z.org"]; kim.Name != "Kim Lee" || kim.UseCount != 1 {
			t.Errorf("kim %+v, want the name from cached mail, count 1", kim)
		}
		jane := got["jane@x.org"]
		if jane.Name != "Jane Doe" || jane.UseCount != 6 || jane.LastUsed != late {
			t.Errorf("jane %+v, want name Jane Doe, count 6, last used %s", jane, late)
		}
		john := got["john@y.org"]
		if john.Name != "" || john.UseCount != 3 || john.LastUsed != early {
			t.Errorf("john %+v, want no name, count 3, last used %s", john, early)
		}
	}
}
