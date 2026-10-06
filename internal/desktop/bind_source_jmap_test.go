package desktop

import (
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// GetMessageSource opened an IMAP connection for every account, which for a
// JMAP account has no host to go to.
func TestGetMessageSourceForJMAP(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertMessage(ctx, &storage.Message{AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", MessageID: "<one@example.com>", Subject: "hi", Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	raw := "Message-ID: <one@example.com>\r\nSubject: hi\r\n\r\nbody"
	fake := &fakeJMAPAdapter{raw: map[string][]byte{"E1": []byte(raw)}}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter { return fake }

	got, err := a.GetMessageSource(id)
	if err != nil {
		t.Fatalf("GetMessageSource: %v", err)
	}
	if got != raw {
		t.Fatalf("source = %q, want the raw message", got)
	}
}
