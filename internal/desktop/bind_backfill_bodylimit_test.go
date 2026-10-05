package desktop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	"github.com/peltonapp/Pelton/internal/storage"
)

// only the list step admits a body window. A body job asking the store would
// count the folder's cached bodies once per job instead of once per folder.
func TestJMAPBackfillCountsBodiesOnlyForListStep(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "bodylimit.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	folder := storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "mb1", RemoteID: "mb1"}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := db.InsertMessage(ctx, &storage.Message{AccountID: accountID, FolderID: folder.ID, RemoteID: id}); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{ctx: ctx, store: db}

	got, err := a.jmapBackfillBodyLimit(ctx, folder, syncsched.JobListStubs, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12 {
		t.Fatalf("list step body limit %d, want 2 cached + 10", got)
	}

	// a closed store fails any query, so a body job that still counts errors.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = a.jmapBackfillBodyLimit(ctx, folder, syncsched.JobFetchBodies, 10)
	if err != nil || got != 0 {
		t.Fatalf("body job = (%d, %v), want (0, nil) without touching the store", got, err)
	}
}
