package desktop

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	pimap "github.com/peltonapp/Pelton/internal/imap"
	"github.com/peltonapp/Pelton/internal/mailexport"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// jmapMoveAccount builds a JMAP account with an inbox and an archive mailbox and
// one message "E1" in the inbox, and wires a fake adapter into the app.
func jmapMoveAccount(t *testing.T, a *App, db *storage.DB, ctx context.Context) (storage.Folder, int64, int64, *fakeJMAPAdapter) {
	t.Helper()
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	archive := &storage.Folder{AccountID: accountID, Name: "Archive", IMAPPath: "mb-archive", RemoteID: "mb-archive", Attributes: []string{`\Archive`}}
	for _, f := range []*storage.Folder{inbox, archive} {
		if _, err := db.CreateFolder(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	id, err := db.InsertMessage(ctx, &storage.Message{AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", MessageID: "<one@example.com>", Subject: "hi", Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeJMAPAdapter{}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter { return fake }
	return *inbox, accountID, id, fake
}

// Archiving opened an IMAP connection for every account, which a JMAP account
// has no host for.
func TestArchiveMessageJMAPMovesThroughTheAdapter(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	inbox, _, id, fake := jmapMoveAccount(t, a, db, ctx)

	undo, err := a.ArchiveMessage(id)
	if err != nil {
		t.Fatalf("ArchiveMessage: %v", err)
	}
	want := []fakeMove{{from: "mb-inbox", to: "mb-archive", ids: []string{"E1"}}}
	if !reflect.DeepEqual(fake.moves, want) {
		t.Errorf("moves = %+v, want %+v", fake.moves, want)
	}
	if _, err := db.GetMessage(ctx, id); err == nil {
		t.Error("the local row survived a successful move")
	}
	if undo.OriginalFolderID != inbox.ID {
		t.Errorf("OriginalFolderID = %d, want %d", undo.OriginalFolderID, inbox.ID)
	}
	if archive, err := a.findArchiveFolder(inbox.AccountID); err != nil || undo.DestFolderID != archive.ID {
		t.Errorf("DestFolderID = %d, want the archive folder (err %v)", undo.DestFolderID, err)
	}
	if undo.RemoteID != "E1" {
		t.Errorf("RemoteID = %q, want E1", undo.RemoteID)
	}
}

// A JMAP email keeps its id across a move, so undo needs no Message-ID and no
// IMAP connection: newIMAPClient stays nil, so any IMAP attempt fails the test.
func TestUndoMoveJMAP(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	inbox, accountID, _, fake := jmapMoveAccount(t, a, db, ctx)
	projects := &storage.Folder{AccountID: accountID, Name: "Projects", IMAPPath: "mb-projects", RemoteID: "mb-projects"}
	if _, err := db.CreateFolder(ctx, projects); err != nil {
		t.Fatal(err)
	}

	if err := a.UnarchiveMessage("", "E1", projects.ID, inbox.ID); err != nil {
		t.Fatalf("UnarchiveMessage: %v", err)
	}
	want := []fakeMove{{from: "mb-projects", to: "mb-inbox", ids: []string{"E1"}}}
	if !reflect.DeepEqual(fake.moves, want) {
		t.Errorf("moves = %+v, want %+v", fake.moves, want)
	}
}

func TestArchiveMessageJMAPKeepsTheRowWhenRefused(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	_, _, id, fake := jmapMoveAccount(t, a, db, ctx)
	fake.failMove = errors.New("forbidden")

	if _, err := a.ArchiveMessage(id); err == nil {
		t.Fatal("ArchiveMessage returned no error although the server refused")
	}
	if _, err := db.GetMessage(ctx, id); err != nil {
		t.Errorf("the row was dropped anyway: %v", err)
	}
}

func TestArchiveMessageJMAPExportsTheSource(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	_, accountID, id, fake := jmapMoveAccount(t, a, db, ctx)
	raw := []byte("Message-ID: <one@example.com>\r\nSubject: hi\r\n\r\nbody")
	fake.raw = map[string][]byte{"E1": raw}
	if err := db.SetAccountArchiveExport(ctx, accountID, true, t.TempDir(), mailexport.SubfoldersNone, ""); err != nil {
		t.Fatal(err)
	}

	undo, err := a.ArchiveMessage(id)
	if err != nil {
		t.Fatalf("ArchiveMessage: %v", err)
	}
	if undo.ExportPath == "" {
		t.Fatalf("no export written, ExportError = %q", undo.ExportError)
	}
	got, err := os.ReadFile(undo.ExportPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Errorf("exported %q, want %q", got, raw)
	}
}

// A JMAP archive must reach the server through the adapter only: dialing IMAP
// for a JMAP account fails the test.
func TestArchiveJMAPNeverDialsIMAP(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	_, _, id, fake := jmapMoveAccount(t, a, db, ctx)
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		t.Fatal("a JMAP account dialed IMAP")
		return nil, nil
	}
	if _, err := a.ArchiveMessage(id); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if len(fake.moves) != 1 {
		t.Fatalf("JMAP moves = %d, want 1", len(fake.moves))
	}
}
