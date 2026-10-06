package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peltonapp/Pelton/internal/mailview"
	"github.com/peltonapp/Pelton/internal/storage"
)

// repairAdapter serves one message and can be told to refuse, which is what a
// message the server no longer has looks like from here.
type repairAdapter struct {
	fakeAdapter
	fetchErr error
	html     string
	atts     []Attachment
}

func (a *repairAdapter) Fetch(_ context.Context, _ string, remoteIDs []string) ([]Fetched, error) {
	a.commands++
	if a.fetchErr != nil {
		for _, id := range remoteIDs {
			a.fetched = append(a.fetched, id)
		}
		return nil, a.fetchErr
	}
	out := make([]Fetched, 0, len(remoteIDs))
	for _, id := range remoteIDs {
		a.fetched = append(a.fetched, id)
		out = append(out, Fetched{
			RemoteID:     id,
			LegacyUID:    1,
			Subject:      "Grüße",
			Text:         "café",
			HTML:         a.html,
			CharsetGuess: "windows-1252",
			Attachments:  a.atts,
		})
	}
	return out, nil
}

func TestSyncRepairsMangledMessages(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)

	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1",
		MessageID: "a@example.com", Subject: "Gr\xfc\xdfe", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}

	adapter := &repairAdapter{ids: fakeIDs(1)}
	res, err := NewEngine(adapter, db, nil).SyncFolder(ctx, folder)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Repaired != 1 || len(res.RepairedIDs) != 1 {
		t.Fatalf("repaired %d (%v), want 1", res.Repaired, res.RepairedIDs)
	}

	fixed, err := db.GetMessage(ctx, broken.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fixed.Subject != "Grüße" || fixed.BodyPlain != "café" {
		t.Errorf("text = %q / %q, want the refetched text", fixed.Subject, fixed.BodyPlain)
	}
	if fixed.CharsetGuess != "windows-1252" {
		t.Errorf("CharsetGuess = %q, want windows-1252", fixed.CharsetGuess)
	}
}

// the message is gone from the server: there is nothing to repair it from, so
// the mark comes off and the next sync does not try again.
func TestSyncStopsRetryingMessagesTheServerLost(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)

	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1",
		MessageID: "a@example.com", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}

	adapter := &repairAdapter{ids: fakeIDs(1), fetchErr: fmt.Errorf("jmap: email %q not found: %w", "1", ErrNotOnServer)}
	if _, err := NewEngine(adapter, db, nil).SyncFolder(ctx, folder); err != nil {
		t.Fatalf("sync: %v", err)
	}

	left, err := db.MessagesNeedingRefetch(ctx, folder.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d messages still marked after a failed refetch", len(left))
	}
}

// The initial sync calls CompleteFolder after stubs and bodies instead of SyncFolder.
// Repair still has to run there.
func TestCompleteFolderRepairsMangledMessages(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)

	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1",
		MessageID: "a@example.com", Subject: "Gr\xfc\xdfe", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}

	adapter := &repairAdapter{ids: fakeIDs(1)}
	res, err := NewEngine(adapter, db, nil).CompleteFolder(ctx, folder)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.Repaired != 1 || len(res.RepairedIDs) != 1 {
		t.Fatalf("repaired %d (%v), want 1", res.Repaired, res.RepairedIDs)
	}
}

// A message cached while the parser dropped Outlook's inline pictures gets them
// back from the refetch; what it already had is left alone.
func TestSyncRepairAddsMissingInlinePictures(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)

	const html = `<img src="cid:image001.png@01DD30BD">`
	stored := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1",
		MessageID: "a@example.com", BodyHTML: html,
	}
	id, err := db.InsertMessageWithAttachments(ctx, &stored, []storage.IncomingAttachment{
		{Filename: "a.pdf", ContentType: "application/pdf", Content: strings.NewReader("pdf")},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n, err := db.MarkMissingInlineParts(ctx, mailview.ReferencedCIDs); err != nil || n != 1 {
		t.Fatalf("mark: %d, %v", n, err)
	}

	adapter := &repairAdapter{ids: fakeIDs(1), html: html, atts: []Attachment{
		{Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("pdf")},
		{Filename: "image001.png", ContentType: "image/png", ContentID: "image001.png@01DD30BD", Content: []byte("png")},
	}}
	res, err := NewEngine(adapter, db, nil).SyncFolder(ctx, folder)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Repaired != 1 {
		t.Fatalf("repaired %d, want 1", res.Repaired)
	}
	atts, err := db.ListAttachments(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("got %d attachments, want the pdf once and the picture", len(atts))
	}
	if left, _ := db.MessagesNeedingRefetch(ctx, folder.ID, 0); len(left) != 0 {
		t.Errorf("%d messages still marked after the repair", len(left))
	}
}

// Opening a marked message repairs it right away instead of waiting for the
// next sync of its folder; other marked messages are left for that sync.
func TestRepairRemoteIDsRepairsOnlyThoseAsked(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)

	var ids []int64
	for uid, rid := range []string{"1", "2"} {
		m := storage.Message{
			AccountID: folder.AccountID, FolderID: folder.ID, UID: uint32(uid + 1), RemoteID: rid,
			MessageID: rid + "@example.com", BodyPlain: "caf\xe9",
		}
		if _, err := db.InsertMessage(ctx, &m); err != nil {
			t.Fatalf("insert: %v", err)
		}
		ids = append(ids, m.ID)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}

	adapter := &repairAdapter{ids: fakeIDs(2)}
	repaired, err := NewEngine(adapter, db, nil).RepairRemoteIDs(ctx, folder, []string{"2", "absent"})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(repaired) != 1 || repaired[0] != ids[1] {
		t.Fatalf("repaired %v, want only message %d", repaired, ids[1])
	}
	if strings.Join(adapter.fetched, ",") != "2" {
		t.Errorf("fetched %v, want only the asked, marked message", adapter.fetched)
	}
	left, err := db.MessagesNeedingRefetch(ctx, folder.ID, 0)
	if err != nil || len(left) != 1 || left[0].ID != ids[0] {
		t.Fatalf("still marked %+v, %v, want message %d", left, err, ids[0])
	}
}

// A refetch that fails for any other reason (offline, a timeout, the server
// busy) keeps the mark: the message is still broken and the next try can work.
func TestRepairKeepsTheMarkOnATransientFailure(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)
	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}

	adapter := &repairAdapter{ids: fakeIDs(1), fetchErr: errors.New("i/o timeout")}
	if _, err := NewEngine(adapter, db, nil).SyncFolder(ctx, folder); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if left, err := db.MessagesNeedingRefetch(ctx, folder.ID, 0); err != nil || len(left) != 1 {
		t.Fatalf("still marked %d, %v, want the mark kept", len(left), err)
	}
}

// An empty answer is how IMAP reports a uid the server no longer has.
func TestRepairClearsTheMarkWhenTheServerReturnsNothing(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)
	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}
	engine := NewEngine(&emptyFetchAdapter{repairAdapter{ids: fakeIDs(1)}}, db, nil)
	if _, err := engine.RepairRemoteIDs(ctx, folder, []string{"1"}); err == nil {
		t.Error("an empty refetch should still report that nothing was repaired")
	}
	if left, _ := db.MessagesNeedingRefetch(ctx, folder.ID, 0); len(left) != 0 {
		t.Errorf("%d still marked, want the mark cleared", len(left))
	}
}

type emptyFetchAdapter struct{ repairAdapter }

func (a *emptyFetchAdapter) Fetch(context.Context, string, []string) ([]Fetched, error) {
	return nil, nil
}

// The open message's refetch reports a failed repair, so the reading pane is
// not told the message changed: it would load it again, see the mark and fetch
// the whole message once more, over and over.
func TestRepairRemoteIDsReportsAFailedRepair(t *testing.T) {
	ctx := context.Background()
	db, folder := newSyncTestFolder(t)
	broken := storage.Message{
		AccountID: folder.AccountID, FolderID: folder.ID, UID: 1, RemoteID: "1", BodyPlain: "caf\xe9",
	}
	if _, err := db.InsertMessage(ctx, &broken); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.MarkMangledMessages(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}
	adapter := &repairAdapter{ids: fakeIDs(1), fetchErr: errors.New("i/o timeout")}
	repaired, err := NewEngine(adapter, db, nil).RepairRemoteIDs(ctx, folder, []string{"1"})
	if err == nil || len(repaired) != 0 {
		t.Fatalf("repaired %v, err %v, want the failure reported", repaired, err)
	}
}
