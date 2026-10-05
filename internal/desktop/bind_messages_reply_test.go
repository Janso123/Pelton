package desktop

import (
	"slices"
	"testing"

	"github.com/peltonapp/Pelton/internal/storage"
)

// TestGetMessageCarriesReplyHeaders: a reply needs the original's Reply-To,
// Message-ID and References, and a message without a chain must give an empty
// list rather than null so the ui can spread it. The stored Message-ID is bare,
// the way the parser leaves it, while References keeps its brackets; the dto
// has to bracket the id so a reply's In-Reply-To is a valid msg-id and matches
// the References entries.
func TestGetMessageCarriesReplyHeaders(t *testing.T) {
	a, db, ctx := phishingTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Local: true})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	folder := &storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, folder); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	insert := func(uid uint32, m storage.Message) int64 {
		t.Helper()
		m.AccountID, m.FolderID, m.UID, m.BodyComplete = accountID, folder.ID, uid, true
		id, err := db.InsertMessage(ctx, &m)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}
	threaded := insert(1, storage.Message{ReplyTo: "list@example.com", MessageID: "m1@x", References: "<r0@x> <r1@x>"})
	bare := insert(2, storage.Message{MessageID: "m2@x"})
	bracketed := insert(3, storage.Message{MessageID: "<m3@x>"})
	noID := insert(4, storage.Message{})

	got, err := a.GetMessage(threaded)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ReplyTo != "list@example.com" || got.MessageIDHeader != "<m1@x>" {
		t.Errorf("ReplyTo = %q, MessageIDHeader = %q", got.ReplyTo, got.MessageIDHeader)
	}
	if want := []string{"<r0@x>", "<r1@x>"}; !slices.Equal(got.References, want) {
		t.Errorf("References = %v, want %v", got.References, want)
	}

	got, err = a.GetMessage(bare)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.References == nil || len(got.References) != 0 {
		t.Errorf("References = %#v, want empty non-nil", got.References)
	}
	if got.MessageIDHeader != "<m2@x>" {
		t.Errorf("MessageIDHeader = %q, want <m2@x>", got.MessageIDHeader)
	}

	for id, want := range map[int64]string{bracketed: "<m3@x>", noID: ""} {
		got, err := a.GetMessage(id)
		if err != nil {
			t.Fatalf("GetMessage: %v", err)
		}
		if got.MessageIDHeader != want {
			t.Errorf("MessageIDHeader = %q, want %q", got.MessageIDHeader, want)
		}
	}
}
