package desktop

import (
	"strings"
	"testing"

	"github.com/peltonapp/Pelton/internal/smtp"
	"github.com/peltonapp/Pelton/internal/storage"
)

// Encrypted mail is stored with no html body; the reader shows the decrypted
// one. Clicking "load remote images" re-renders through GetMessageHTML, which
// used to read the empty stored body and blank the message.
func TestGetMessageHTMLUsesTheDecryptedBody(t *testing.T) {
	app, db, accountID := pgpTestApp(t, []string{"me@example.com"}, nil)
	req := ComposeRequest{
		AccountID:  accountID,
		To:         addr("me@example.com"),
		Subject:    "s",
		Text:       "t",
		HTML:       `<p>secret <img src="https://img.example/x.png"></p>`,
		Protection: protectionEncrypt,
	}
	msg, err := app.buildMessage(req)
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	account, err := db.GetAccount(app.ctx, accountID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	mode, opts, engine, err := app.protectionOptions(*account, req)
	if err != nil {
		t.Fatalf("protectionOptions: %v", err)
	}
	raw, err := smtp.BuildRaw(msg, engine, mode, opts)
	if err != nil {
		t.Fatalf("BuildRaw: %v", err)
	}

	folderID, err := db.CreateFolder(app.ctx, &storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	id, err := db.InsertMessage(app.ctx, &storage.Message{AccountID: accountID, FolderID: folderID, UID: 1, MessageID: "<e@example.com>"})
	if err != nil {
		t.Fatalf("InsertMessage: %v", err)
	}
	if err := db.SetMessagePGPSource(app.ctx, id, raw); err != nil {
		t.Fatalf("SetMessagePGPSource: %v", err)
	}

	html, err := app.GetMessageHTML(id, true, false)
	if err != nil {
		t.Fatalf("GetMessageHTML: %v", err)
	}
	if !strings.Contains(html, "secret") {
		t.Fatalf("GetMessageHTML = %q, want the decrypted body", html)
	}
}
