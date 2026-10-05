package desktop

import (
	"context"
	"errors"
	"testing"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

func onlyAccount(t *testing.T, a *App) storage.Account {
	t.Helper()
	list, err := a.store.ListAccounts(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []storage.Account
	for _, acc := range list {
		if !acc.Local {
			out = append(out, acc)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want 1 account, got %d", len(out))
	}
	return out[0]
}

func fakeJMAPAuth(a *App, session, mailID string) {
	a.authenticateTarget = func(_ context.Context, _ storage.Account, _ proxy.Config, protocol string, _ credentials.Secret) (targetAuth, error) {
		if protocol != "jmap" {
			return targetAuth{}, errors.New("unexpected protocol")
		}
		return targetAuth{SessionURL: session, MailAccountID: mailID}, nil
	}
}

func TestBackupRoundTripKeepsJMAPProtocolAndParallel(t *testing.T) {
	a := newAccountTestApp(t)
	n := 2
	acc := storage.Account{Email: "bob@example.org", IMAPHost: "mail.example.org", IMAPPort: 993, Protocol: "jmap",
		JMAPSessionURL: "https://mail.example.org/.well-known/jmap", JMAPMailAccountID: "acc1"}
	id, err := a.store.CreateAccount(a.ctx, &acc)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetAccountSyncMaxParallel(a.ctx, id, &n); err != nil {
		t.Fatal(err)
	}
	secret := credentials.Secret{Method: credentials.MethodPassword, Password: "pw"}
	if err := credentials.Store(id, secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credentials.Delete(id) })

	boxes, err := a.exportMailboxes("export-pass")
	if err != nil {
		t.Fatal(err)
	}
	if boxes[0].Protocol != "jmap" || boxes[0].SyncMaxParallel == nil || *boxes[0].SyncMaxParallel != 2 {
		t.Fatalf("export lost fields: %+v", boxes[0])
	}

	b := newAccountTestApp(t)
	fakeJMAPAuth(b, "https://mail.example.org/.well-known/jmap", "acc1")
	if _, err := b.importMailboxes(boxes, "export-pass"); err != nil {
		t.Fatal(err)
	}
	got := onlyAccount(t, b)
	t.Cleanup(func() { _ = credentials.Delete(got.ID) })
	if got.Protocol != "jmap" || got.SyncMaxParallel == nil || *got.SyncMaxParallel != 2 {
		t.Fatalf("import lost fields: %+v", got)
	}
	if got.JMAPSessionURL == "" || got.JMAPMailAccountID != "acc1" {
		t.Fatalf("session fields not filled: %+v", got)
	}
}

func TestImportOldBackupWithoutProtocolIsIMAP(t *testing.T) {
	b := newAccountTestApp(t)
	old := []mailboxBackup{{Email: "a@example.org", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 465}}
	if _, err := b.importMailboxes(old, ""); err != nil {
		t.Fatal(err)
	}
	if got := onlyAccount(t, b); got.Protocol != "imap" || got.SyncMaxParallel != nil {
		t.Fatalf("got %+v, want imap with no override", got)
	}
}

func TestImportUnknownProtocolIsIMAP(t *testing.T) {
	b := newAccountTestApp(t)
	boxes := []mailboxBackup{{Email: "a@example.org", IMAPHost: "h", IMAPPort: 993, Protocol: "pop3"}}
	if _, err := b.importMailboxes(boxes, ""); err != nil {
		t.Fatal(err)
	}
	if got := onlyAccount(t, b); got.Protocol != "imap" {
		t.Fatalf("protocol = %q, want imap", got.Protocol)
	}
}

// Switching protocol drops the cache and downloads everything again, so a JMAP
// backup restored without its password stays JMAP. jmapClient finds the
// session on the first sync after the password is entered.
func TestImportJMAPBackupWithoutCredentialsStaysJMAP(t *testing.T) {
	b := newAccountTestApp(t)
	b.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		t.Fatal("must not authenticate without a secret")
		return targetAuth{}, nil
	}
	n := 9
	boxes := []mailboxBackup{{Email: "a@example.org", IMAPHost: "h", IMAPPort: 993, Protocol: "jmap", SyncMaxParallel: &n}}
	if _, err := b.importMailboxes(boxes, ""); err != nil {
		t.Fatal(err)
	}
	got := onlyAccount(t, b)
	if got.Protocol != "jmap" || got.JMAPSessionURL != "" || got.JMAPMailAccountID != "" {
		t.Fatalf("got %+v, want jmap with no session yet", got)
	}
	if _, err := credentials.Load(got.ID); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("credential load err = %v, want none stored", err)
	}
	if got.SyncMaxParallel == nil || *got.SyncMaxParallel != maxSyncMaxParallel {
		t.Fatalf("override = %v, want clamped %d", got.SyncMaxParallel, maxSyncMaxParallel)
	}
}

func TestImportJMAPBackupAuthFailureStaysJMAP(t *testing.T) {
	b := newAccountTestApp(t)
	b.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, errors.New("down")
	}
	blob, err := encryptWithPassword("p", []byte(`{"method":"password","password":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	boxes := []mailboxBackup{{Email: "a@example.org", IMAPHost: "h", IMAPPort: 993, Protocol: "jmap", Secret: blob}}
	if _, err := b.importMailboxes(boxes, "p"); err != nil {
		t.Fatal(err)
	}
	got := onlyAccount(t, b)
	t.Cleanup(func() { _ = credentials.Delete(got.ID) })
	if got.Protocol != "jmap" || got.JMAPSessionURL != "" {
		t.Fatalf("got %+v, want jmap with no session yet", got)
	}
	secret, err := credentials.Load(got.ID)
	if err != nil || secret.Password != "x" {
		t.Fatalf("credential = %+v, %v; want the restored password", secret, err)
	}
}

// The sign-in during a restore takes the route the account will sync over (a
// restored account follows the global proxy), never a direct connection.
func TestImportJMAPBackupAuthenticatesOverTheGlobalProxy(t *testing.T) {
	b := newAccountTestApp(t)
	b.proxyMu.Lock()
	b.proxyCfg = proxy.Config{Mode: "manual", Scheme: "socks5", Host: "127.0.0.1", Port: 1080}
	b.proxyMu.Unlock()
	var got proxy.Config
	b.authenticateTarget = func(_ context.Context, _ storage.Account, route proxy.Config, _ string, _ credentials.Secret) (targetAuth, error) {
		got = route
		return targetAuth{SessionURL: "https://x/jmap", MailAccountID: "a"}, nil
	}
	blob, err := encryptWithPassword("p", []byte(`{"method":"password","password":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	boxes := []mailboxBackup{{Email: "a@example.org", IMAPHost: "h", IMAPPort: 993, Protocol: "jmap", Secret: blob}}
	if _, err := b.importMailboxes(boxes, "p"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credentials.Delete(onlyAccount(t, b).ID) })
	if got != b.currentProxy() {
		t.Fatalf("route = %+v, want global %+v", got, b.currentProxy())
	}
}
