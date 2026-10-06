package desktop

import (
	"fmt"
	"sync"
	"testing"
)

// The frontend holds draft ids as JavaScript numbers. An id it cannot hold
// exactly comes back rounded, and saving or deleting with it used to report
// success while touching nothing.
func TestDraftIDsSurviveAJavaScriptNumber(t *testing.T) {
	app, _, accountID := pgpTestApp(t, nil, nil)

	id, err := app.SaveDraft(0, ComposeRequest{AccountID: accountID, Subject: "one"})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	jsID := int64(float64(id))
	if jsID != id {
		t.Fatalf("draft id %d does not survive a float64 round trip (%d)", id, jsID)
	}
	if _, err := app.SaveDraft(jsID, ComposeRequest{AccountID: accountID, Subject: "two"}); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	drafts, err := app.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(drafts) != 1 || drafts[0].Request.Subject != "two" {
		t.Fatalf("drafts = %+v, want one draft with the re-saved subject", drafts)
	}
	if err := app.DeleteDraft(jsID); err != nil {
		t.Fatalf("DeleteDraft: %v", err)
	}
	if drafts, _ := app.ListDrafts(); len(drafts) != 0 {
		t.Fatalf("drafts after delete = %+v, want none", drafts)
	}
}

func TestLegacyDraftIDsAreRenumbered(t *testing.T) {
	app, db, accountID := pgpTestApp(t, nil, nil)
	legacy := []storedDraft{
		{ID: 3, SavedAt: "2026-10-01T10:00:00Z", Request: ComposeRequest{AccountID: accountID, Subject: "small"}, AccountID: accountID},
		{ID: 1791136856123456789, SavedAt: "2026-10-01T11:00:00Z", Request: ComposeRequest{AccountID: accountID, Subject: "big"}, AccountID: accountID},
	}
	if err := db.SetJSON(app.ctx, draftsKey, legacy); err != nil {
		t.Fatalf("seed drafts: %v", err)
	}

	drafts, err := app.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(drafts) != 2 {
		t.Fatalf("got %d drafts, want 2", len(drafts))
	}
	var bigID int64
	for _, d := range drafts {
		if d.ID > maxSafeDraftID {
			t.Errorf("draft %q kept unsafe id %d", d.Request.Subject, d.ID)
		}
		if d.Request.Subject == "small" && d.ID != 3 {
			t.Errorf("a safe id was renumbered to %d", d.ID)
		}
		if d.Request.Subject == "big" {
			bigID = d.ID
		}
	}
	if _, err := app.SaveDraft(bigID, ComposeRequest{AccountID: accountID, Subject: "big edited"}); err != nil {
		t.Fatalf("save under the renumbered id: %v", err)
	}
}

// A compose pane whose draft was discarded elsewhere still holds the old id.
// Saving it must keep what the user wrote, as a new draft under a new id the
// pane takes over, rather than failing every save from then on.
func TestSaveDraftWithUnknownIDCreatesANewDraft(t *testing.T) {
	app, _, accountID := pgpTestApp(t, nil, nil)
	kept, err := app.SaveDraft(0, ComposeRequest{AccountID: accountID, Subject: "kept"})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	const gone = 42
	id, err := app.SaveDraft(gone, ComposeRequest{AccountID: accountID, Subject: "orphan"})
	if err != nil {
		t.Fatalf("SaveDraft(unknown) = %v, want a new draft", err)
	}
	if id == gone || id == kept || id <= 0 || id > maxSafeDraftID {
		t.Fatalf("SaveDraft(unknown) returned id %d, want a new safe id", id)
	}
	drafts, err := app.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	subjects := map[int64]string{}
	for _, d := range drafts {
		subjects[d.ID] = d.Request.Subject
	}
	if len(drafts) != 2 || subjects[kept] != "kept" || subjects[id] != "orphan" {
		t.Fatalf("drafts = %v, want kept under %d and orphan under %d", subjects, kept, id)
	}
}

// Two compose panes autosaving at once must not lose a draft to each other's
// load, modify, write of the one drafts value.
func TestConcurrentDraftSavesKeepEveryDraft(t *testing.T) {
	app, _, accountID := pgpTestApp(t, nil, nil)

	const panes = 20
	var wg sync.WaitGroup
	errs := make(chan error, panes)
	for i := range panes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := app.SaveDraft(0, ComposeRequest{AccountID: accountID, Subject: fmt.Sprintf("draft %d", i)})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
	}

	drafts, err := app.ListDrafts()
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(drafts) != panes {
		t.Fatalf("got %d drafts, want %d", len(drafts), panes)
	}
	ids := map[int64]bool{}
	for _, d := range drafts {
		ids[d.ID] = true
	}
	if len(ids) != panes {
		t.Fatalf("got %d distinct ids, want %d", len(ids), panes)
	}
}
