package storage

import (
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// cidRefs stands in for mailview.ReferencedCIDs: lowercased ids of every cid:
// url in the html.
func cidRefs(html string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`cid:[^"'\s>)]+`).FindAllString(html, -1) {
		out[strings.ToLower(strings.TrimPrefix(m, "cid:"))] = true
	}
	return out
}

func TestMarkMissingInlinePartsFindsOnlyMessagesMissingAPicture(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	accountID, err := db.CreateAccount(ctx, &Account{Email: "a@example.com"})
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	folder := &Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := db.CreateFolder(ctx, folder); err != nil {
		t.Fatalf("folder: %v", err)
	}

	insert := func(uid uint32, html string, atts ...IncomingAttachment) int64 {
		t.Helper()
		id, err := db.InsertMessageWithAttachments(ctx, &Message{
			AccountID: accountID, FolderID: folder.ID, UID: uid, BodyHTML: html,
		}, atts)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}
	// the stored content id keeps its brackets and case; the html does not.
	whole := insert(1, `<img src="cid:Logo@Example">`,
		IncomingAttachment{Filename: "logo.png", ContentType: "image/png", ContentID: "<logo@example>", Content: strings.NewReader("png")})
	missing := insert(2, `<img src="cid:image001.png@01DD"><img src="cid:image002.png@01DD">`,
		IncomingAttachment{Filename: "a.pdf", ContentType: "application/pdf", Content: strings.NewReader("pdf")},
		IncomingAttachment{Filename: "image002.png", ContentType: "image/png", ContentID: "image002.png@01DD", Content: strings.NewReader("png")})
	plain := insert(3, `<p>no pictures</p>`)
	// a stub has no body yet; fetching it will bring its parts anyway.
	if _, err := db.UpsertMessageListMeta(ctx, &Message{
		AccountID: accountID, FolderID: folder.ID, RemoteID: "stub", BodyHTML: `<img src="cid:x">`,
	}); err != nil {
		t.Fatalf("stub: %v", err)
	}

	found, err := db.MarkMissingInlineParts(ctx, cidRefs)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if found != 1 {
		t.Errorf("marked %d messages, want 1", found)
	}
	marked, err := db.MessagesNeedingRefetch(ctx, folder.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(marked) != 1 || marked[0].ID != missing {
		t.Fatalf("marked %+v, want only message %d (whole=%d plain=%d)", marked, missing, whole, plain)
	}
	if again, err := db.MarkMissingInlineParts(ctx, cidRefs); err != nil || again != 0 {
		t.Errorf("second scan found %d, %v, want 0 and no error", again, err)
	}
}

// Local Folders mail has no server to fetch from, so it is never marked: the
// mark would only keep a spinner on that nothing can turn off.
func TestMarkMissingInlinePartsSkipsLocalFolders(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	account, _ := db.EnsureLocalAccount(ctx)
	folder, _ := db.EnsureLocalFolder(ctx, account.ID, "Archive")
	if _, err := db.InsertMessage(ctx, &Message{
		AccountID: account.ID, FolderID: folder.ID, UID: 1, BodyHTML: `<img src="cid:x@y">`,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if found, err := db.MarkMissingInlineParts(ctx, cidRefs); err != nil || found != 0 {
		t.Fatalf("marked %d, %v, want 0 for a local message", found, err)
	}
}

func TestAddMissingInlineAttachmentsKeepsWhatIsStored(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	account, _ := db.EnsureLocalAccount(ctx)
	folder, _ := db.EnsureLocalFolder(ctx, account.ID, "Inbox")

	id, err := db.InsertMessageWithAttachments(ctx, &Message{
		AccountID: account.ID, FolderID: folder.ID, UID: 1, BodyHTML: `<img src="cid:image001.png@01DD">`,
	}, []IncomingAttachment{
		{Filename: "a.pdf", ContentType: "application/pdf", Content: strings.NewReader("pdf")},
		{Filename: "image002.png", ContentType: "image/png", ContentID: "image002.png@01DD", Content: strings.NewReader("old")},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	added, err := db.AddMissingInlineAttachments(ctx, id, []IncomingAttachment{
		{Filename: "a.pdf", ContentType: "application/pdf", Content: strings.NewReader("pdf")},
		{Filename: "image001.png", ContentType: "image/png", ContentID: "<Image001.png@01DD>", Content: strings.NewReader("new")},
		{Filename: "image002.png", ContentType: "image/png", ContentID: "image002.png@01DD", Content: strings.NewReader("dup")},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added != 1 {
		t.Errorf("added %d, want only the missing picture", added)
	}

	atts, err := db.ListAttachments(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("got %d attachments, want the pdf, the old picture and the new one", len(atts))
	}
	var got *Attachment
	for i := range atts {
		if atts[i].Filename == "image001.png" {
			got = &atts[i]
		}
	}
	if got == nil || got.ContentID != "<Image001.png@01DD>" || got.ContentType != "image/png" {
		t.Fatalf("new attachment %+v", got)
	}
	rc, err := db.OpenAttachment(got.DiskPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "new" {
		t.Errorf("content %q, want new", b)
	}
}

// The sync's repair and an opened message's refetch can fill the same message
// at once. Each picture is still stored a single time.
func TestAddMissingInlineAttachmentsConcurrentFillsStoreOnce(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	account, _ := db.EnsureLocalAccount(ctx)
	folder, _ := db.EnsureLocalFolder(ctx, account.ID, "Inbox")
	m := Message{AccountID: account.ID, FolderID: folder.ID, UID: 1, BodyHTML: `<img src="cid:a@b">`}
	if _, err := db.InsertMessage(ctx, &m); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := db.AddMissingInlineAttachments(ctx, m.ID, []IncomingAttachment{
				{Filename: "a.png", ContentType: "image/png", ContentID: "a@b", Content: strings.NewReader("png")},
			}); err != nil {
				t.Errorf("add: %v", err)
			}
		})
	}
	wg.Wait()
	atts, err := db.ListAttachments(ctx, m.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("stored %d copies of the picture, want 1", len(atts))
	}
}
