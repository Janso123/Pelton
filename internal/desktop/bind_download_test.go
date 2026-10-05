package desktop

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	pimap "github.com/peltonapp/Pelton/internal/imap"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// downloadPGPRaw is a minimal PGP/MIME encrypted message, so the test can see
// whether the download kept the source decryption needs.
const downloadPGPRaw = "Message-ID: <enc@example.com>\r\n" +
	"Subject: secret\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=b\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: application/pgp-encrypted\r\n" +
	"\r\n" +
	"Version: 1\r\n" +
	"--b\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"\r\n" +
	"-----BEGIN PGP MESSAGE-----\r\n" +
	"hQEMA\r\n" +
	"-----END PGP MESSAGE-----\r\n" +
	"--b--\r\n"

// JMAP sync lists every email as a stub, so a bulk download that skipped rows
// already in the cache never fetched a JMAP body, and it went through IMAP,
// which a JMAP account has no host for. The stub must be filled through the
// account's adapter, keep its PGP source, and end up pinned; the message that
// already has a body is only pinned.
func TestDownloadFillsJMAPStubsAndPinsCompleteMessages(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	stubID, err := db.UpsertMessageListMeta(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", Subject: "secret", Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	completeID, err := db.InsertMessage(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, RemoteID: "E2", Subject: "hi",
		BodyPlain: "body", Date: time.Now(), BodyComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeJMAPAdapter{raw: map[string][]byte{"E1": []byte(downloadPGPRaw)}}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter { return fake }

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if len(tasks) != 1 || tasks[0].remoteID != "E1" || tasks[0].folder.ID != inbox.ID {
		t.Fatalf("tasks %+v, want only E1 in the inbox", tasks)
	}
	if !slices.Equal(pin, []int64{completeID}) {
		t.Fatalf("pin %v, want [%d]", pin, completeID)
	}

	if err := a.runDownload(ctx, tasks, pin, false, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	stub, err := db.GetMessage(ctx, stubID)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.BodyComplete || !stub.Offline {
		t.Errorf("E1 body complete %v offline %v, want both", stub.BodyComplete, stub.Offline)
	}
	src, err := db.MessagePGPSource(ctx, stubID)
	if err != nil {
		t.Fatalf("pgp source: %v", err)
	}
	if !bytes.Equal(src, []byte(downloadPGPRaw)) {
		t.Errorf("E1 pgp source %q, want the raw message", src)
	}
	complete, err := db.GetMessage(ctx, completeID)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Offline {
		t.Error("E2 already had its body but was not pinned offline")
	}
}

// IMAP still asks the server what is in range, since the cache may not hold
// every uid. A uid already cached with its body is only pinned; one missing
// from the cache is fetched, stored and pinned.
func TestDownloadPlansAndFetchesIMAPUIDs(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	cachedID, err := db.InsertMessage(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, UID: 1, Subject: "cached",
		BodyPlain: "body", Date: time.Now(), BodyComplete: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeIMAP{
		since: []imap.UID{1, 2},
		messages: map[imap.UID]*pimap.Message{
			2: {UID: 2, Subject: "new", Text: "fresh body", Date: time.Now()},
		},
	}
	useFake(t, a, accountID, client)

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if len(tasks) != 1 || tasks[0].remoteID != "2" {
		t.Fatalf("tasks %+v, want only uid 2", tasks)
	}
	if !slices.Equal(pin, []int64{cachedID}) {
		t.Fatalf("pin %v, want [%d]", pin, cachedID)
	}

	if err := a.runDownload(context.Background(), tasks, pin, true, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	states, err := db.MessageBodyStates(ctx, inbox.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var fetchedID int64
	for _, s := range states {
		if s.UID == 2 {
			fetchedID = s.ID
		}
	}
	if fetchedID == 0 {
		t.Fatal("uid 2 was not stored")
	}
	fetched, err := db.GetMessage(ctx, fetchedID)
	if err != nil {
		t.Fatal(err)
	}
	if !fetched.BodyComplete || !fetched.Offline || fetched.BodyPlain != "fresh body" {
		t.Errorf("uid 2 body complete %v offline %v body %q", fetched.BodyComplete, fetched.Offline, fetched.BodyPlain)
	}
	cached, err := db.GetMessage(ctx, cachedID)
	if err != nil {
		t.Fatal(err)
	}
	if !cached.Offline {
		t.Error("uid 1 was cached with its body but not pinned offline")
	}
}

// downloadAttachmentRaw is a message with one attachment, to see whether a
// download kept it.
const downloadAttachmentRaw = "Message-ID: <att@example.com>\r\n" +
	"Subject: with file\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"see attached\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain\r\n" +
	"Content-Disposition: attachment; filename=\"notes.txt\"\r\n" +
	"\r\n" +
	"attached text\r\n" +
	"--b--\r\n"

// With "include attachments" off, a stub that is already in the cache must
// still get its attachments: the row is the message the user sees in the list,
// and once its body is complete nothing fetches it again, so leaving them out
// would lose them for good.
func TestDownloadWithoutAttachmentsStillFillsAStubWithThem(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	stubID, err := db.UpsertMessageListMeta(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", Subject: "with file", Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter {
		return &fakeJMAPAdapter{raw: map[string][]byte{"E1": []byte(downloadAttachmentRaw)}}
	}

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if err := a.runDownload(ctx, tasks, pin, false, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	atts, err := db.ListAttachments(ctx, stubID)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].Filename != "notes.txt" {
		t.Errorf("stub attachments %+v, want notes.txt", atts)
	}
}

// For IMAP the same holds for a uid cached without its body, while a uid not
// cached at all is stored without attachments, as the user asked.
func TestDownloadWithoutAttachmentsSkipsThemOnlyForUncachedMessages(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	stubID, err := db.UpsertMessageListMeta(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, UID: 3, Subject: "stub", Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	file := []pimap.Attachment{{Filename: "notes.txt", ContentType: "text/plain", Content: []byte("attached text")}}
	client := &fakeIMAP{
		since: []imap.UID{2, 3},
		messages: map[imap.UID]*pimap.Message{
			2: {UID: 2, Subject: "new", Text: "fresh", Date: time.Now(), Attachments: file},
			3: {UID: 3, Subject: "stub", Text: "filled", Date: time.Now(), Attachments: file},
		},
	}
	useFake(t, a, accountID, client)

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if err := a.runDownload(ctx, tasks, pin, false, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	states, err := db.MessageBodyStates(ctx, inbox.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var newID int64
	for _, s := range states {
		if s.UID == 2 {
			newID = s.ID
		}
	}
	if newID == 0 {
		t.Fatal("uid 2 was not stored")
	}
	for id, want := range map[int64]int{stubID: 1, newID: 0} {
		atts, err := db.ListAttachments(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(atts) != want {
			t.Errorf("message %d has %d attachments, want %d", id, len(atts), want)
		}
	}
}

// Body sync can store a planned message between the plan and the fetch. The
// download then finds the row complete and stores nothing, but the message is
// still in the range the user asked to keep offline, so it has to be pinned.
func TestDownloadPinsAMessageWhoseBodyLandedAfterThePlan(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	stubID, err := db.UpsertMessageListMeta(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", Subject: "hi", Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter {
		return &fakeJMAPAdapter{raw: map[string][]byte{"E1": []byte(downloadAttachmentRaw)}}
	}

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if _, err := db.InsertMessageWithAttachments(ctx, &storage.Message{
		AccountID: accountID, FolderID: inbox.ID, RemoteID: "E1", Subject: "hi", BodyPlain: "synced", Date: time.Now(),
	}, nil); err != nil {
		t.Fatalf("body sync fill: %v", err)
	}
	if err := a.runDownload(ctx, tasks, pin, true, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	got, err := db.GetMessage(ctx, stubID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Offline {
		t.Error("a message whose body landed after the plan was not pinned offline")
	}
}

// A JMAP bulk download can run for minutes. Holding the account lock for all
// of it made a protocol switch on that account fail on its lock wait, and the
// download kept going on the old protocol's ids. It takes the lock per batch
// and stops once a switch holds the account.
func TestJMAPDownloadLocksPerBatchAndStopsWhenTheAccountIsHeld(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	accountID, err := db.CreateAccount(ctx, &storage.Account{Email: "me@example.com", Protocol: "jmap", JMAPMailAccountID: "A1"})
	if err != nil {
		t.Fatal(err)
	}
	inbox := &storage.Folder{AccountID: accountID, Name: "Inbox", IMAPPath: "mb-inbox", RemoteID: "mb-inbox"}
	if _, err := db.CreateFolder(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	raw := map[string][]byte{}
	for i := range downloadBatch * 2 {
		id := fmt.Sprintf("E%d", i)
		if _, err := db.UpsertMessageListMeta(ctx, &storage.Message{
			AccountID: accountID, FolderID: inbox.ID, RemoteID: id, Subject: id, Date: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		raw[id] = []byte("Subject: " + id + "\r\n\r\nbody\r\n")
	}
	fetches := 0
	var release func()
	fake := &fakeJMAPAdapter{raw: raw}
	fake.onFetch = func() {
		fetches++
		if a.accountLock(accountID).TryLock() {
			a.accountLock(accountID).Unlock()
			t.Error("Fetch ran without the account lock")
		}
		if fetches == 1 {
			// a protocol switch starts while the first batch downloads.
			release = a.holdAccountSync(accountID)
		}
	}
	a.jmapAdapterForTest = func(storage.Account) psync.Adapter { return fake }
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})

	tasks, pin, err := a.planDownload(ctx, time.Now().AddDate(0, -1, 0))
	if err != nil {
		t.Fatalf("planDownload: %v", err)
	}
	if len(tasks) != downloadBatch*2 {
		t.Fatalf("planned %d tasks, want %d", len(tasks), downloadBatch*2)
	}
	if err := a.runDownload(ctx, tasks, pin, false, len(tasks)); err != nil {
		t.Fatalf("runDownload: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("Fetch ran %d times, want the download to stop after the batch the switch interrupted", fetches)
	}
	if !a.accountLock(accountID).TryLock() {
		t.Fatal("the account lock is still held after the download stopped")
	}
	a.accountLock(accountID).Unlock()
}
