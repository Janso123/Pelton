package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/mcpserver"
	"github.com/peltonapp/Pelton/internal/search"
	"github.com/peltonapp/Pelton/internal/storage"
)

// searchProfileApp builds an app with two accounts, one indexed "invoice"
// message each, and returns the message ids in account order.
func searchProfileApp(t *testing.T) (a *App, db *storage.DB, ctx context.Context, accounts, messages [2]int64) {
	t.Helper()
	a, db, ctx = moveTestApp(t)
	idx, err := search.Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	a.index = idx

	for n, email := range []string{"a@example.com", "b@example.com"} {
		acc, err := db.CreateAccount(ctx, &storage.Account{Email: email})
		if err != nil {
			t.Fatalf("create account: %v", err)
		}
		folder := &storage.Folder{AccountID: acc, Name: "INBOX", IMAPPath: "INBOX"}
		if _, err := db.CreateFolder(ctx, folder); err != nil {
			t.Fatalf("create folder: %v", err)
		}
		id, err := db.InsertMessage(ctx, &storage.Message{
			AccountID: acc, FolderID: folder.ID, UID: 1, MessageID: "<m@example.com>",
			Subject: "invoice", Date: time.Now(),
		})
		if err != nil {
			t.Fatalf("insert message: %v", err)
		}
		doc := search.Doc{ID: id, AccountID: acc, FolderID: folder.ID, Subject: "invoice", Date: time.Now()}
		if err := idx.IndexDoc(doc); err != nil {
			t.Fatalf("index: %v", err)
		}
		accounts[n], messages[n] = acc, id
	}
	return a, db, ctx, accounts, messages
}

// Search ran over every account in the index, so mail from accounts the active
// profile does not contain showed up in its results.
func TestSearchStaysWithinProfileAccounts(t *testing.T) {
	a, db, ctx, accounts, messages := searchProfileApp(t)
	main, err := db.MainProfile(ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}
	if err := db.SetProfileAccounts(ctx, main.ID, []int64{accounts[0]}); err != nil {
		t.Fatalf("set profile accounts: %v", err)
	}

	res, err := a.Search(SearchRequestDTO{Query: "invoice"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total != 1 || len(res.Messages) != 1 || res.Messages[0].ID != messages[0] {
		t.Fatalf("got %+v total %d, want only message %d", res.Messages, res.Total, messages[0])
	}
}

// A profile that holds no accounts has no mail to search, which is not the same
// as having no filter.
func TestSearchProfileWithoutAccountsFindsNothing(t *testing.T) {
	a, db, ctx, _, _ := searchProfileApp(t)
	main, err := db.MainProfile(ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}
	if err := db.SetProfileAccounts(ctx, main.ID, nil); err != nil {
		t.Fatalf("set profile accounts: %v", err)
	}

	res, err := a.Search(SearchRequestDTO{Query: "invoice"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Messages) != 0 || res.Total != 0 {
		t.Fatalf("got %+v total %d, want nothing", res.Messages, res.Total)
	}
}

// hideSecondAccount limits the main profile to the first account.
func hideSecondAccount(t *testing.T, db *storage.DB, ctx context.Context, visible int64) {
	t.Helper()
	main, err := db.MainProfile(ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}
	var ids []int64
	if visible != 0 {
		ids = []int64{visible}
	}
	if err := db.SetProfileAccounts(ctx, main.ID, ids); err != nil {
		t.Fatalf("set profile accounts: %v", err)
	}
}

// Select-all over search results ran against every indexed account, so it could
// select (and then act on) mail the profile does not show.
func TestSearchMessageIDsStaysWithinProfileAccounts(t *testing.T) {
	a, db, ctx, accounts, messages := searchProfileApp(t)
	hideSecondAccount(t, db, ctx, accounts[0])

	got, err := a.SearchMessageIDs(SearchRequestDTO{Query: "invoice"})
	if err != nil {
		t.Fatalf("search ids: %v", err)
	}
	if len(got.IDs) != 1 || got.IDs[0] != messages[0] {
		t.Fatalf("got %v, want only %d", got.IDs, messages[0])
	}
}

func TestSearchMessageIDsProfileWithoutAccountsFindsNothing(t *testing.T) {
	a, db, ctx, _, _ := searchProfileApp(t)
	hideSecondAccount(t, db, ctx, 0)

	got, err := a.SearchMessageIDs(SearchRequestDTO{Query: "invoice"})
	if err != nil {
		t.Fatalf("search ids: %v", err)
	}
	if got.IDs == nil || len(got.IDs) != 0 {
		t.Fatalf("got %#v, want an empty id list", got.IDs)
	}
}

func TestMCPSearchStaysWithinProfileAccounts(t *testing.T) {
	a, db, ctx, accounts, messages := searchProfileApp(t)
	hideSecondAccount(t, db, ctx, accounts[0])
	mb := &mcpMailbox{app: a}

	got, err := mb.Search(ctx, mcpserver.SearchParams{Query: "invoice"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != messages[0] {
		t.Fatalf("got %+v, want only message %d", got, messages[0])
	}

	hideSecondAccount(t, db, ctx, 0)
	got, err = mb.Search(ctx, mcpserver.SearchParams{Query: "invoice"})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty profile: got %+v, %v, want nothing", got, err)
	}
}

// The MCP read tools took ids straight from the agent, so a hidden account's
// message, folder list or folder contents were one guess away.
func TestMCPReadsRefuseAccountsOutsideProfile(t *testing.T) {
	a, db, ctx, accounts, messages := searchProfileApp(t)
	hideSecondAccount(t, db, ctx, accounts[0])
	mb := &mcpMailbox{app: a}

	if _, err := mb.GetMessage(ctx, messages[0]); err != nil {
		t.Fatalf("visible message: %v", err)
	}
	if _, err := mb.GetMessage(ctx, messages[1]); !errors.Is(err, storage.ErrMessageNotFound) {
		t.Fatalf("hidden message: got %v, want ErrMessageNotFound", err)
	}

	hiddenMsg, err := db.GetMessage(ctx, messages[1])
	if err != nil {
		t.Fatalf("load hidden message: %v", err)
	}
	if _, err := mb.ListMessages(ctx, hiddenMsg.FolderID, 10); !errors.Is(err, storage.ErrFolderNotFound) {
		t.Fatalf("hidden folder: got %v, want ErrFolderNotFound", err)
	}
	if _, err := mb.ListFolders(ctx, accounts[1]); !errors.Is(err, storage.ErrAccountNotFound) {
		t.Fatalf("hidden account folders: got %v, want ErrAccountNotFound", err)
	}
	if folders, err := mb.ListFolders(ctx, accounts[0]); err != nil || len(folders) == 0 {
		t.Fatalf("visible folders: %v, %v", folders, err)
	}
}

// The MCP write tools took ids straight from the agent, so an agent could
// mark, move, delete or propose mail for an account the active profile hides.
func TestMCPWritesRefuseAccountsOutsideProfile(t *testing.T) {
	a, db, ctx, accounts, messages := searchProfileApp(t)
	hideSecondAccount(t, db, ctx, accounts[0])
	w := &mcpWriter{app: a}
	hidden := messages[1]

	for name, call := range map[string]func() error{
		"mark read":  func() error { return w.MarkRead(ctx, hidden, true) },
		"archive":    func() error { return w.Archive(ctx, hidden) },
		"flag":       func() error { return w.Flag(ctx, hidden, true) },
		"flag color": func() error { return w.SetFlagColor(ctx, hidden, 2) },
		"delete":     func() error { return w.Delete(ctx, hidden) },
		"move": func() error {
			visible, err := db.GetMessage(ctx, messages[0])
			if err != nil {
				t.Fatalf("load visible message: %v", err)
			}
			return w.Move(ctx, hidden, visible.FolderID)
		},
	} {
		if err := call(); !errors.Is(err, storage.ErrMessageNotFound) {
			t.Errorf("%s on a hidden message: got %v, want ErrMessageNotFound", name, err)
		}
	}
	msg, err := db.GetMessage(ctx, hidden)
	if err != nil {
		t.Fatalf("hidden message: %v", err)
	}
	if msg.Flags != 0 {
		t.Errorf("hidden message flags changed to %v", msg.Flags)
	}

	hiddenMsg, err := db.GetMessage(ctx, hidden)
	if err != nil {
		t.Fatalf("load hidden message: %v", err)
	}
	if err := w.Move(ctx, messages[0], hiddenMsg.FolderID); !errors.Is(err, storage.ErrFolderNotFound) {
		t.Errorf("move into a hidden folder: got %v, want ErrFolderNotFound", err)
	}

	out := mcpserver.OutgoingMessage{To: []string{"x@example.com"}, Subject: "hi"}
	out.AccountID = accounts[1]
	if _, err := w.QueueSend(ctx, out); !errors.Is(err, storage.ErrAccountNotFound) {
		t.Errorf("propose from a hidden account: got %v, want ErrAccountNotFound", err)
	}
	out.AccountID = accounts[0]
	if _, err := w.QueueSend(ctx, out); err != nil {
		t.Errorf("propose from a visible account: %v", err)
	}
	proposals, err := db.ListAgentProposals(ctx)
	if err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	if len(proposals) != 1 || proposals[0].AccountID != accounts[0] {
		t.Errorf("proposals = %+v, want one from the visible account", proposals)
	}
}
