package desktop

import (
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/search"
	"github.com/peltonapp/Pelton/internal/storage"
)

// addIndexedFolder stores and indexes one "invoice" message in a new folder of
// the account and returns the folder and message ids.
func addIndexedFolder(t *testing.T, a *App, db *storage.DB, account int64, path string, flags storage.Flag) (folder, message int64) {
	t.Helper()
	ctx := a.ctx
	f := &storage.Folder{AccountID: account, Name: path, IMAPPath: path}
	if _, err := db.CreateFolder(ctx, f); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	id, err := db.InsertMessage(ctx, &storage.Message{
		AccountID: account, FolderID: f.ID, UID: 1, MessageID: "<" + path + "@example.com>",
		Subject: "invoice", Date: time.Now(), Flags: flags,
	})
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := a.index.IndexDoc(search.Doc{ID: id, AccountID: account, FolderID: f.ID, Subject: "invoice", Date: time.Now()}); err != nil {
		t.Fatalf("index: %v", err)
	}
	return f.ID, id
}

func resultIDs(res SearchResultDTO) []int64 {
	out := make([]int64, 0, len(res.Messages))
	for _, m := range res.Messages {
		out = append(out, m.ID)
	}
	return out
}

// A search started in a folder used to run over every folder of the profile,
// so the inbox's results were mostly junk.
func TestSearchStaysInFolder(t *testing.T) {
	a, db, _, accounts, messages := searchProfileApp(t)
	junk, junkMsg := addIndexedFolder(t, a, db, accounts[0], "Junk", 0)

	inbox, err := a.store.GetMessage(a.ctx, messages[0])
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	res, err := a.Search(SearchRequestDTO{Query: "invoice", FolderID: inbox.FolderID})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := resultIDs(res); len(got) != 1 || got[0] != messages[0] || res.Total != 1 {
		t.Fatalf("inbox search = %v total %d, want only %d", got, res.Total, messages[0])
	}

	res, err = a.Search(SearchRequestDTO{Query: "invoice", FolderID: junk})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := resultIDs(res); len(got) != 1 || got[0] != junkMsg {
		t.Fatalf("junk search = %v, want only %d", got, junkMsg)
	}

	// no scope is every folder, which is what the "all folders" chip asks for.
	res, err = a.Search(SearchRequestDTO{Query: "invoice"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("unscoped search = %v, want all three", resultIDs(res))
	}
}

// A search started in a unified view covers that view's folders across
// accounts and nothing else.
func TestSearchStaysInUnifiedView(t *testing.T) {
	a, db, _, accounts, messages := searchProfileApp(t)
	addIndexedFolder(t, a, db, accounts[0], "Junk", 0)

	res, err := a.Search(SearchRequestDTO{Query: "invoice", View: viewInbox})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := resultIDs(res); len(got) != 2 || res.Total != 2 {
		t.Fatalf("unified inbox search = %v total %d, want both inbox messages %v", got, res.Total, messages)
	}

	ids, err := a.SearchMessageIDs(SearchRequestDTO{Query: "invoice", View: viewInbox})
	if err != nil {
		t.Fatalf("search ids: %v", err)
	}
	if len(ids.IDs) != 2 {
		t.Fatalf("select all in unified inbox = %v, want the two inbox messages", ids.IDs)
	}
}

// The flagged view is every folder narrowed to flagged mail, so a search from
// it has to keep the flag as well as the folders.
func TestSearchInFlaggedViewKeepsOnlyFlagged(t *testing.T) {
	a, db, _, accounts, _ := searchProfileApp(t)
	_, flagged := addIndexedFolder(t, a, db, accounts[0], "Projects", storage.FlagFlagged)

	res, err := a.Search(SearchRequestDTO{Query: "invoice", View: viewFlagged})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := resultIDs(res); len(got) != 1 || got[0] != flagged {
		t.Fatalf("flagged search = %v, want only %d", got, flagged)
	}

	ids, err := a.SearchMessageIDs(SearchRequestDTO{Query: "invoice", View: viewFlagged})
	if err != nil {
		t.Fatalf("search ids: %v", err)
	}
	if len(ids.IDs) != 1 || ids.IDs[0] != flagged {
		t.Fatalf("select all in flagged = %v, want only %d", ids.IDs, flagged)
	}
}

// A unified view with no folder behind it finds nothing rather than falling
// back to every folder.
func TestSearchInEmptyUnifiedViewFindsNothing(t *testing.T) {
	a, _, _, _, _ := searchProfileApp(t)
	res, err := a.Search(SearchRequestDTO{Query: "invoice", View: viewJunk})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Messages) != 0 || res.Total != 0 {
		t.Fatalf("junk view search = %v total %d, want nothing", resultIDs(res), res.Total)
	}
}
