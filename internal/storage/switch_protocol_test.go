package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedSwitchAccount creates an account with one folder, n bulk-inserted
// messages (FTS triggers fire) and attachments on the first few of them.
func seedSwitchAccount(t *testing.T, db *DB, email string, n, withFiles int) (accountID, folderID int64, files []string, msgIDs []int64) {
	t.Helper()
	ctx := context.Background()
	accountID, err := db.CreateAccount(ctx, &Account{Email: email, IMAPHost: "imap.example", IMAPPort: 993})
	if err != nil {
		t.Fatal(err)
	}
	f := &Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, f); err != nil {
		t.Fatal(err)
	}
	folderID = f.ID
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (account_id, folder_id, uid, subject, body_plain, remote_id) VALUES (?, ?, ?, ?, ?, ?)`,
			accountID, folderID, i, "subject "+strings.Repeat("word ", 10), strings.Repeat("body text number ", 400), "r"+email+"-"+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := range withFiles {
		id, err := db.InsertMessageWithAttachments(ctx, &Message{
			AccountID: accountID, FolderID: folderID, UID: uint32(n + 1 + i), Subject: "att",
		}, []IncomingAttachment{{Filename: "a.txt", ContentType: "text/plain", Content: strings.NewReader("x")}})
		if err != nil {
			t.Fatal(err)
		}
		msgIDs = append(msgIDs, id)
		files = append(files, filepath.Join(db.AttachmentsDir(), accountSegment(accountID), messageSegment(id), "a.txt"))
	}
	return accountID, folderID, files, msgIDs
}

func countRows(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSwitchAccountProtocolRemovesOnlyThatAccountsCache(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, aFolder, aFiles, _ := seedSwitchAccount(t, db, "a@example.com", 700, 3)
	b, bFolder, bFiles, _ := seedSwitchAccount(t, db, "b@example.com", 50, 2)
	if _, err := db.InsertMessage(ctx, &Message{
		AccountID: a, FolderID: aFolder, UID: 9000, Subject: "zanzibarquokka",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.SwitchAccountProtocol(ctx, a, "jmap", "https://j.example/session", "acc1", nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE account_id = ?`, a); n != 0 {
		t.Fatalf("a messages left = %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM folders WHERE account_id = ?`, a); n != 0 {
		t.Fatalf("a folders left = %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM attachments WHERE message_id NOT IN (SELECT id FROM messages)`); n != 0 {
		t.Fatalf("orphan attachment rows = %d", n)
	}
	// messages_fts is external-content, so COUNT(*) reads the messages table.
	// MATCH and integrity-check read the index itself.
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'zanzibarquokka'`); n != 0 {
		t.Fatalf("fts still matches %d of a's messages", n)
	}
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("fts integrity-check: %v", err)
	}
	for _, p := range aFiles {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("a file still present: %s (%v)", p, err)
		}
	}
	for _, p := range bFiles {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("b file lost: %v", err)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE folder_id = ?`, bFolder); n != 52 {
		t.Fatalf("b messages = %d, want 52", n)
	}
	acc, err := db.GetAccount(ctx, a)
	if err != nil || acc.Protocol != "jmap" || acc.JMAPSessionURL != "https://j.example/session" {
		t.Fatalf("account = %+v, err %v", acc, err)
	}
	if entries, _ := os.ReadDir(db.AttachmentsDir()); len(entries) != 1 {
		t.Fatalf("attachments dir entries = %d (staging left over?), want only b: %v", len(entries), entries)
	}
	_ = b
}

func TestSwitchAccountProtocolDoesNotStallOtherAccountWrites(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// The race detector slows SQLite work ~20x, so shrink the data and widen
	// the allowed stall there (only SQLITE_BUSY is checked); the plain run uses the full 6000 messages.
	n, limit := 6000, 200*time.Millisecond
	if raceEnabled {
		n, limit = 1500, 14*time.Second
	}
	a, _, _, _ := seedSwitchAccount(t, db, "a@example.com", n, 5)
	b, bFolder, _, _ := seedSwitchAccount(t, db, "b@example.com", 10, 0)

	var (
		stop     atomic.Bool
		wg       sync.WaitGroup
		maxStall atomic.Int64
		writes   atomic.Int64
		firstErr atomic.Value
	)
	wg.Go(func() {
		for uid := uint32(1000); !stop.Load(); uid++ {
			start := time.Now()
			_, err := db.UpsertMessageListMeta(ctx, &Message{AccountID: b, FolderID: bFolder, UID: uid, Subject: "new"})
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			writes.Add(1)
			if d := time.Since(start).Nanoseconds(); d > maxStall.Load() {
				maxStall.Store(d)
			}
			time.Sleep(2 * time.Millisecond)
		}
	})

	begin := time.Now()
	err := db.SwitchAccountProtocol(ctx, a, "jmap", "https://j.example/session", "acc1", nil)
	took := time.Since(begin)
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if v := firstErr.Load(); v != nil {
		t.Fatalf("concurrent writer failed: %v", v)
	}
	stall := time.Duration(maxStall.Load())
	t.Logf("switch took %v, %d concurrent writes, max write stall %v", took, writes.Load(), stall)
	if stall > limit {
		t.Fatalf("a write on another account stalled %v during the switch; the switch holds the write lock too long", stall)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM messages WHERE account_id = ?`, a); n != 0 {
		t.Fatalf("a messages left = %d", n)
	}
}

// a switch that fails while clearing the cache leaves the folders behind. They
// must not keep a sync floor: with sync_initialized cleared but a floor still
// set, the next sync fetched the whole mailbox instead of the initial limit.
func TestSwitchAccountProtocolFailureLeavesNoFloor(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, folder, _, _ := seedSwitchAccount(t, db, "a@example.com", 10, 0)
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE folders SET sync_floor_uid = 5, sync_floor_id = 'm05', sync_initialized = 1 WHERE id = ?`, folder); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderFullSyncAt(ctx, folder, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`CREATE TRIGGER fail_message_delete BEFORE DELETE ON messages BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}

	if err := db.SwitchAccountProtocol(ctx, a, "jmap", "https://j.example/session", "acc1", nil); err == nil {
		t.Fatal("switch succeeded with message deletes failing")
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM folders WHERE id = ? AND sync_floor_uid = 0 AND sync_floor_id = '' AND sync_initialized = 0`, folder); n != 1 {
		var uid int
		var id string
		_ = db.sql.QueryRow(`SELECT sync_floor_uid, sync_floor_id FROM folders WHERE id = ?`, folder).Scan(&uid, &id)
		t.Fatalf("folder floor after failed switch = (%d, %q), want (0, \"\")", uid, id)
	}
	got, err := db.GetFolder(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastFullSyncAt.IsZero() {
		t.Fatalf("LastFullSyncAt after failed switch = %v, want zero", got.LastFullSyncAt)
	}
}

// The progress callback fires once for each committed message batch, and not
// for a batch that rolled back.
func TestSwitchAccountProtocolReportsEachCommittedBatch(t *testing.T) {
	t.Run("every batch commits", func(t *testing.T) {
		db := newTestDB(t)
		a, _, _, _ := seedSwitchAccount(t, db, "a@example.com", 700, 0)
		var batches int
		if err := db.SwitchAccountProtocol(context.Background(), a, "jmap", "https://j.example/session", "acc1",
			func() { batches++ }); err != nil {
			t.Fatal(err)
		}
		// 700 messages at switchDeleteBatch (300) a batch.
		if batches != 3 {
			t.Fatalf("progress callbacks = %d, want 3", batches)
		}
	})
	t.Run("second batch rolls back", func(t *testing.T) {
		db := newTestDB(t)
		a, _, _, _ := seedSwitchAccount(t, db, "a@example.com", 700, 0)
		var txs int
		db.TestingSetDeleteTxHook(func(context.Context) error {
			txs++
			if txs == 2 {
				return errors.New("inject fail")
			}
			return nil
		})
		var batches int
		if err := db.SwitchAccountProtocol(context.Background(), a, "jmap", "https://j.example/session", "acc1",
			func() { batches++ }); err == nil {
			t.Fatal("expected the injected failure")
		}
		if batches != 1 {
			t.Fatalf("progress callbacks = %d, want 1", batches)
		}
	})
}
