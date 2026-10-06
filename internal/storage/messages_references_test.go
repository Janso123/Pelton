package storage

import (
	"context"
	"testing"
)

func TestReferencesRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	accountID, err := db.CreateAccount(ctx, &Account{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	folder := Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "mb1", RemoteID: "mb1"}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}

	id, err := db.InsertMessage(ctx, &Message{
		AccountID: accountID, FolderID: folder.ID, RemoteID: "full",
		Subject: "Full", References: "<a@x> <b@y>",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMessage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.References != "<a@x> <b@y>" {
		t.Fatalf("References = %q", m.References)
	}
}

func TestReferencesWrittenWhenStubIsFilled(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	accountID, err := db.CreateAccount(ctx, &Account{Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	folder := Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "mb1", RemoteID: "mb1"}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertMessageListMeta(ctx, &Message{
		AccountID: accountID, FolderID: folder.ID, RemoteID: "1", Subject: "Hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	filled, err := db.InsertMessageWithAttachments(ctx, &Message{
		AccountID: accountID, FolderID: folder.ID, RemoteID: "1", Subject: "Hi",
		BodyPlain: "body", References: "<a@x> <b@y>",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filled != id {
		t.Fatalf("stub %d was not filled in place (got %d)", id, filled)
	}
	m, err := db.GetMessage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.References != "<a@x> <b@y>" {
		t.Fatalf("References = %q", m.References)
	}
}
