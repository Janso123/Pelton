package desktop

import (
	"strings"
	"testing"

	"github.com/peltonapp/Pelton/internal/storage"
)

// Outlook content ids carry upper-case hex ("image001.png@01DD30BD.0D1ADF40")
// and cid: urls are matched lowercased, so the stored id has to be lowercased
// too or the picture stays an empty box.
func TestRenderHTMLInlinesMixedCaseContentIDs(t *testing.T) {
	a, add := newTrustTestApp(t)
	html := `<img src="cid:image001.png@01DD30BD.0D1ADF40">`
	id := add("a@example.com")
	m, err := a.store.GetMessage(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.AddMissingInlineAttachments(a.ctx, id, []storage.IncomingAttachment{{
		Filename: "image001.png", ContentType: "image/png", ContentID: "<image001.png@01DD30BD.0D1ADF40>",
		Content: strings.NewReader("png"),
	}}); err != nil {
		t.Fatal(err)
	}
	atts, err := a.store.ListAttachments(a.ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := a.renderHTML(html, atts, false)
	if strings.Contains(got, "cid:") || !strings.Contains(got, "data:image/png;base64,") {
		t.Fatalf("rendered %q, want the picture inlined as a data url", got)
	}
}

// One pass at start marks mail cached while the parser dropped inline pictures,
// and records that it ran so later starts skip the scan.
func TestMarkMissingInlineMailRunsOnce(t *testing.T) {
	a, add := newTrustTestApp(t)
	id := add("a@example.com")
	if err := a.store.RepairMessageText(a.ctx, id, "", "", `<img src="cid:x@y">`, ""); err != nil {
		t.Fatal(err)
	}
	m := storage.Message{ID: id}

	a.markMissingInlineMail()
	if !a.needsRefetch(m.ID) {
		t.Fatal("a message missing its inline picture was not marked")
	}
	if err := a.store.ClearRefetchMark(a.ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	a.markMissingInlineMail()
	if a.needsRefetch(m.ID) {
		t.Error("the scan ran again on a later start")
	}
}
