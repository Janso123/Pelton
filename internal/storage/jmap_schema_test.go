package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJMAPSchemaBackfillAndUniqueness(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	id, err := db.CreateAccount(ctx, &Account{Email: "a@example.com", IMAPHost: "imap.example", IMAPPort: 993})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != "imap" || got.JMAPSessionURL != "" || got.JMAPMailAccountID != "" {
		t.Fatalf("account = %+v, want imap and empty jmap fields", got)
	}

	folder := &Folder{AccountID: id, Name: "INBOX", IMAPPath: "INBOX", UIDValidity: 7}
	if _, err := db.CreateFolder(ctx, folder); err != nil {
		t.Fatal(err)
	}
	if folder.RemoteID != "INBOX" {
		t.Fatalf("remote id = %q, want INBOX", folder.RemoteID)
	}

	msgID, err := db.InsertMessage(ctx, &Message{AccountID: id, FolderID: folder.ID, UID: 42, Subject: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	states, err := db.ListMessageStates(ctx, folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].RemoteID != "42" || states[0].ID != msgID {
		t.Fatalf("states = %+v", states)
	}

	// two JMAP rows in one folder both use uid 0 and do not collide
	jmapFolder := &Folder{AccountID: id, Name: "Inbox", IMAPPath: "Mb1", RemoteID: "Mb1"}
	if _, err := db.CreateFolder(ctx, jmapFolder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, &Message{AccountID: id, FolderID: jmapFolder.ID, UID: 0, RemoteID: "E1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, &Message{AccountID: id, FolderID: jmapFolder.ID, UID: 0, RemoteID: "E2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertMessage(ctx, &Message{AccountID: id, FolderID: jmapFolder.ID, UID: 0, RemoteID: "123"}); err != nil {
		t.Fatal(err)
	}
	// Numeric-looking JMAP ids are still opaque and leave every UID field zero.
	if _, err := db.InsertMessage(ctx, &Message{AccountID: id, FolderID: jmapFolder.ID, UID: 0, RemoteID: "E1"}); err == nil {
		t.Fatal("duplicate remote id was accepted")
	}

	if err := db.UpdateAccountProtocol(ctx, id, "jmap", "https://example.com/.well-known/jmap", "mail-1"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != "jmap" || got.JMAPSessionURL != "https://example.com/.well-known/jmap" || got.JMAPMailAccountID != "mail-1" {
		t.Fatalf("updated account = %+v", got)
	}

}

func TestJMAPMigration0037PreservesRefsAndFTS(t *testing.T) {
	ctx := context.Background()
	db := openDBThrough(t, 36)

	var accountID, folderID, messageID int64
	if err := db.sql.QueryRowContext(ctx,
		`INSERT INTO accounts (email, imap_host, imap_port, created_at) VALUES (?, ?, ?, ?) RETURNING id`,
		"pre@example.com", "imap.example", 993, nowText()).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.sql.QueryRowContext(ctx,
		`INSERT INTO folders (account_id, name, imap_path) VALUES (?, ?, ?) RETURNING id`,
		accountID, "INBOX", "INBOX").Scan(&folderID); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if err := db.sql.QueryRowContext(ctx, `
INSERT INTO messages (account_id, folder_id, uid, subject, body_plain, from_address)
VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		accountID, folderID, 99, "uniquejmapftssubject", "body with uniquejmapftsbody token", "from@example.com").Scan(&messageID); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO attachments (message_id, filename, disk_path) VALUES (?, ?, ?)`,
		messageID, "a.txt", "1/99/a.txt"); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO message_pgp (message_id, raw) VALUES (?, ?)`,
		messageID, []byte("pgp-raw")); err != nil {
		t.Fatalf("insert pgp: %v", err)
	}
	if n := ftsMatchCount(t, db, "uniquejmapftssubject"); n != 1 {
		t.Fatalf("pre-migration fts hits = %d, want 1", n)
	}

	if err := applyMigrationVersion(ctx, db, 37); err != nil {
		t.Fatalf("apply 0037: %v", err)
	}

	var fkRows int
	fkCheck, err := db.sql.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	for fkCheck.Next() {
		fkRows++
	}
	fkCheck.Close()
	if fkRows != 0 {
		t.Fatalf("foreign_key_check rows = %d, want 0", fkRows)
	}

	var remoteID string
	if err := db.sql.QueryRowContext(ctx, `SELECT remote_id FROM messages WHERE id = ?`, messageID).Scan(&remoteID); err != nil {
		t.Fatalf("read remote_id: %v", err)
	}
	if remoteID != "99" {
		t.Fatalf("remote_id = %q, want 99", remoteID)
	}
	var attCount, pgpCount int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&attCount); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_pgp WHERE message_id = ?`, messageID).Scan(&pgpCount); err != nil {
		t.Fatal(err)
	}
	if attCount != 1 || pgpCount != 1 {
		t.Fatalf("refs attachment=%d pgp=%d, want 1/1", attCount, pgpCount)
	}

	triggers := listFTSTriggers(t, db)
	wantTriggers := []string{"messages_fts_ad", "messages_fts_ai", "messages_fts_au"}
	if len(triggers) != len(wantTriggers) {
		t.Fatalf("triggers = %v, want %v", triggers, wantTriggers)
	}
	for i, name := range wantTriggers {
		if triggers[i] != name {
			t.Fatalf("triggers = %v, want %v", triggers, wantTriggers)
		}
	}
	if n := ftsMatchCount(t, db, "uniquejmapftssubject"); n != 1 {
		t.Fatalf("post-migration fts hits = %d, want 1", n)
	}

	var newID int64
	if err := db.sql.QueryRowContext(ctx, `
INSERT INTO messages (account_id, folder_id, uid, remote_id, subject, body_plain)
VALUES (?, ?, 0, 'E-new', 'brandnewjmapphrase', 'x') RETURNING id`,
		accountID, folderID).Scan(&newID); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if n := ftsMatchCount(t, db, "brandnewjmapphrase"); n != 1 {
		t.Fatalf("insert fts hits = %d, want 1", n)
	}
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE messages SET subject = 'updatedjmapphrase' WHERE id = ?`, newID); err != nil {
		t.Fatalf("update after migration: %v", err)
	}
	if n := ftsMatchCount(t, db, "brandnewjmapphrase"); n != 0 {
		t.Fatalf("old subject still indexed: %d", n)
	}
	if n := ftsMatchCount(t, db, "updatedjmapphrase"); n != 1 {
		t.Fatalf("update fts hits = %d, want 1", n)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, newID); err != nil {
		t.Fatalf("delete after migration: %v", err)
	}
	if n := ftsMatchCount(t, db, "updatedjmapphrase"); n != 0 {
		t.Fatalf("delete left fts hits = %d", n)
	}
}

func TestStagedCacheDeletion(t *testing.T) {
	ctx := context.Background()

	t.Run("message success", func(t *testing.T) {
		db, accountID, folderID, msgID, path := seedCachedAttachment(t)
		if err := db.DeleteCachedMessage(ctx, accountID, msgID); err != nil {
			t.Fatal(err)
		}
		assertGone(t, db, path, msgID)
		_ = folderID
	})

	t.Run("message rollback restores", func(t *testing.T) {
		db, accountID, _, msgID, path := seedCachedAttachment(t)
		db.deleteTxHook = func(context.Context) error { return errors.New("inject fail") }
		if err := db.DeleteCachedMessage(ctx, accountID, msgID); err == nil {
			t.Fatal("expected injected failure")
		}
		assertPresent(t, db, path, msgID)
	})

	t.Run("folder success", func(t *testing.T) {
		db, accountID, folderID, msgID, path := seedCachedAttachment(t)
		n, err := db.PurgeFolderMessages(ctx, accountID, folderID)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("purged = %d, want 1", n)
		}
		assertGone(t, db, path, msgID)
	})

	t.Run("folder rollback restores", func(t *testing.T) {
		db, accountID, folderID, msgID, path := seedCachedAttachment(t)
		db.deleteTxHook = func(context.Context) error { return errors.New("inject fail") }
		if _, err := db.PurgeFolderMessages(ctx, accountID, folderID); err == nil {
			t.Fatal("expected injected failure")
		}
		assertPresent(t, db, path, msgID)
	})

	t.Run("account success", func(t *testing.T) {
		db, accountID, folderID, msgID, path := seedCachedAttachment(t)
		if err := db.DeleteAccountFolders(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.GetFolder(ctx, folderID); !errors.Is(err, ErrFolderNotFound) {
			t.Fatalf("folder err = %v, want not found", err)
		}
		assertGone(t, db, path, msgID)
	})

	t.Run("account rollback restores", func(t *testing.T) {
		db, accountID, folderID, msgID, path := seedCachedAttachment(t)
		db.deleteTxHook = func(context.Context) error { return errors.New("inject fail") }
		if err := db.DeleteAccountFolders(ctx, accountID); err == nil {
			t.Fatal("expected injected failure")
		}
		if _, err := db.GetFolder(ctx, folderID); err != nil {
			t.Fatalf("folder should remain: %v", err)
		}
		assertPresent(t, db, path, msgID)
	})
}

func ftsMatchCount(t *testing.T, db *DB, term string) int {
	t.Helper()
	var n int
	err := db.sql.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&n)
	if err != nil {
		t.Fatalf("fts match %q: %v", term, err)
	}
	return n
}

func listFTSTriggers(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.sql.QueryContext(context.Background(), `
SELECT name FROM sqlite_master
WHERE type = 'trigger' AND name IN ('messages_fts_ai', 'messages_fts_ad', 'messages_fts_au')
ORDER BY name`)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func seedCachedAttachment(t *testing.T) (db *DB, accountID, folderID, msgID int64, filePath string) {
	t.Helper()
	db = newTestDB(t)
	ctx := context.Background()
	accountID, err := db.CreateAccount(ctx, &Account{Email: "cache@example.com", IMAPHost: "imap.example", IMAPPort: 993})
	if err != nil {
		t.Fatal(err)
	}
	folder := &Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, folder); err != nil {
		t.Fatal(err)
	}
	folderID = folder.ID
	msgID, err = db.InsertMessageWithAttachments(ctx, &Message{
		AccountID: accountID, FolderID: folderID, UID: 7, Subject: "cached",
	}, []IncomingAttachment{{
		Filename: "note.txt", ContentType: "text/plain", Content: strings.NewReader("hello"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	filePath = filepath.Join(db.AttachmentsDir(), accountSegment(accountID), messageSegment(msgID), "note.txt")
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}
	return db, accountID, folderID, msgID, filePath
}

func assertGone(t *testing.T, db *DB, path string, msgID int64) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
	if _, err := db.GetMessage(context.Background(), msgID); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("message err = %v, want not found", err)
	}
}

func assertPresent(t *testing.T, db *DB, path string, msgID int64) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file missing after rollback: %v", err)
	}
	if _, err := db.GetMessage(context.Background(), msgID); err != nil {
		t.Fatalf("message missing after rollback: %v", err)
	}
}

// A session found on first connect lands on a JMAP account only: an account
// switched to IMAP while the session was being looked for keeps its protocol.
func TestSetAccountJMAPSession(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	jmapID, err := db.CreateAccount(ctx, &Account{Email: "j@example.com", IMAPHost: "imap.example", IMAPPort: 993, Protocol: "jmap"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountJMAPSession(ctx, jmapID, "https://example.com/.well-known/jmap", "mail-1"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAccount(ctx, jmapID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != "jmap" || got.JMAPSessionURL != "https://example.com/.well-known/jmap" || got.JMAPMailAccountID != "mail-1" {
		t.Fatalf("account = %+v", got)
	}

	imapID, err := db.CreateAccount(ctx, &Account{Email: "i@example.com", IMAPHost: "imap.example", IMAPPort: 993})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountJMAPSession(ctx, imapID, "https://example.com/.well-known/jmap", "mail-1"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound for an imap account", err)
	}
	got, err = db.GetAccount(ctx, imapID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != "imap" || got.JMAPSessionURL != "" || got.JMAPMailAccountID != "" {
		t.Fatalf("imap account changed: %+v", got)
	}
}
