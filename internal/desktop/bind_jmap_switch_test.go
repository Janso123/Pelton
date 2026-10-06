package desktop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
	"golang.org/x/oauth2"
)

func newJMAPSwitchTestApp(t *testing.T) *App {
	t.Helper()
	ctx, stopBackground := testContext(t)
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(stopBackground)
	if err := store.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	session, cancel := context.WithCancel(ctx)
	app := &App{
		ctx:         ctx,
		session:     session,
		sessionStop: cancel,
		store:       store,
		log:         slog.New(slog.DiscardHandler),
		workers:     make(map[int64]*accountWorker),
		pending:     make(map[string]pendingAccount),
	}
	// Cancel the session and join workers before other cleanups (credentials,
	// store) so an idle/watch loop cannot race the mock keyring on teardown.
	t.Cleanup(func() {
		cancel()
		app.joinAllAccountWorkers()
	})
	return app
}

func seedSwitchAccount(t *testing.T, a *App, email, protocol string) (accountID, folderID int64) {
	t.Helper()
	ctx := a.ctx
	acc := &storage.Account{
		Email: email, IMAPHost: "imap.example.test", IMAPPort: 993,
		Protocol: protocol,
	}
	if protocol == "jmap" {
		acc.JMAPSessionURL = "https://jmap.example/.well-known/jmap"
		acc.JMAPMailAccountID = "mail-1"
	}
	id, err := a.store.CreateAccount(ctx, acc)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := credentials.Store(id, credentials.Secret{
		Method: credentials.MethodPassword, Password: "secret",
	}); err != nil {
		t.Fatalf("store secret: %v", err)
	}
	// Stop the worker before deleting the secret: cleanups run LIFO, so this
	// runs before newJMAPSwitchTestApp's join, and SwitchProtocol may have left
	// a replacement worker still reading the keyring.
	t.Cleanup(func() {
		a.stopAccountWorker(id)
		_ = credentials.Delete(id)
	})

	folder := &storage.Folder{
		AccountID: id, Name: "INBOX", IMAPPath: "INBOX", RemoteID: "INBOX",
	}
	fid, err := a.store.CreateFolder(ctx, folder)
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}
	if _, err := a.store.InsertMessage(ctx, &storage.Message{
		AccountID: id, FolderID: fid, UID: 1, Subject: "hello",
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := a.store.CreateAddressBook(ctx, &storage.AddressBook{
		AccountID: id, Name: "Personal", URL: "https://dav.example",
		CollectionPath: "/books/personal/", Username: email,
	}); err != nil {
		t.Fatalf("create address book: %v", err)
	}
	return id, fid
}

func TestSwitchProtocolFailedLoginLeavesCache(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "user@example.test", "jmap")

	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, errors.New("login refused")
	}

	if err := a.SwitchProtocol(id, "imap"); err == nil {
		t.Fatal("expected auth failure")
	}

	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Protocol != "jmap" {
		t.Fatalf("protocol = %q, want jmap", acc.Protocol)
	}
	if _, err := a.store.GetFolder(a.ctx, folderID); err != nil {
		t.Fatalf("folder should remain: %v", err)
	}
	books, err := a.store.ListAddressBooks(a.ctx)
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %v/%d, want 1", err, len(books))
	}
}

func TestSwitchProtocolToIMAPClearsFoldersKeepsBooks(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "user@example.test", "jmap")

	otherID, err := a.store.CreateAccount(a.ctx, &storage.Account{
		Email: "other@example.test", IMAPHost: "imap.example.test", IMAPPort: 993,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherFolder, err := a.store.CreateFolder(a.ctx, &storage.Folder{
		AccountID: otherID, Name: "INBOX", IMAPPath: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.InsertOutbox(a.ctx, storage.OutboxRow{
		AccountID: id, EnvelopeFrom: "user@example.test",
		Recipients: "a@b.test", Raw: []byte("raw"), State: "queued",
	}); err != nil {
		t.Fatal(err)
	}

	a.authenticateTarget = func(_ context.Context, _ storage.Account, _ proxy.Config, protocol string, _ credentials.Secret) (targetAuth, error) {
		if protocol != "imap" {
			t.Fatalf("unexpected protocol %q", protocol)
		}
		return targetAuth{}, nil
	}

	if err := a.SwitchProtocol(id, "imap"); err != nil {
		t.Fatalf("SwitchProtocol: %v", err)
	}

	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Protocol != "imap" {
		t.Fatalf("protocol = %q, want imap", acc.Protocol)
	}
	if acc.JMAPSessionURL != "" || acc.JMAPMailAccountID != "" {
		t.Fatalf("jmap fields should be cleared: %+v", acc)
	}
	if _, err := a.store.GetFolder(a.ctx, folderID); !errors.Is(err, storage.ErrFolderNotFound) {
		t.Fatalf("switched account folder err = %v, want not found", err)
	}
	if _, err := a.store.GetFolder(a.ctx, otherFolder); err != nil {
		t.Fatalf("other account folder should remain: %v", err)
	}
	books, err := a.store.ListAddressBooks(a.ctx)
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %v/%d, want 1", err, len(books))
	}
	rows, err := a.store.ListOutbox(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.AccountID == id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("outbox count = %d, want 1", n)
	}
}

func TestTestConnectionProbe(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		return &fakeIMAP{}, nil
	}

	orig := probeDomain
	t.Cleanup(func() { probeDomain = orig })

	t.Run("available", func(t *testing.T) {
		probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
			return pjmap.Probe{
				Available: true, WebSocket: true,
				SessionURL: "https://jmap.example/.well-known/jmap", MailAccountID: "A1",
			}, nil
		}
		got, err := a.TestConnection(TestConnectionRequest{
			Email: "u@example.test", IMAPHost: "imap.example", IMAPPort: 993, Password: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !got.JMAPAvailable || !got.JMAPWebSocket || got.JMAPSessionURL == "" || got.JMAPMailAccountID != "A1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("probe error", func(t *testing.T) {
		probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
			return pjmap.Probe{}, errors.New("probe failed")
		}
		got, err := a.TestConnection(TestConnectionRequest{
			Email: "u@example.test", IMAPHost: "imap.example", IMAPPort: 993, Password: "x",
		})
		if err != nil {
			t.Fatalf("IMAP succeeded so error must be nil, got %v", err)
		}
		if got.JMAPAvailable {
			t.Fatalf("got %+v, want unavailable", got)
		}
	})
}

func TestCancelAddAccount(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	a.oauthAuthorize = func(context.Context, string, string, string, string, func(string)) (*oauth2.Token, error) {
		return &oauth2.Token{
			AccessToken: "access", RefreshToken: "refresh",
			Expiry: time.Now().Add(time.Hour),
		}, nil
	}
	origProbe := probeDomain
	t.Cleanup(func() { probeDomain = origProbe })
	probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
		return pjmap.Probe{Available: true, SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}

	pending, err := a.BeginOAuthAccount(AddAccountRequest{
		Email: "oauth@example.test", Provider: "google", ClientID: "cid",
		IMAPHost: "imap.example", IMAPPort: 993,
	})
	if err != nil {
		t.Fatalf("BeginOAuthAccount: %v", err)
	}
	if pending.ID == "" || !pending.JMAPAvailable {
		t.Fatalf("pending = %+v", pending)
	}
	if err := a.CancelAddAccount(pending.ID); err != nil {
		t.Fatal(err)
	}
	accounts, err := a.store.ListAllAccounts(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, acc := range accounts {
		if acc.Email == "oauth@example.test" {
			t.Fatal("CancelAddAccount must not create an account row")
		}
	}
}

func TestSwitchProtocolRejectsIneligibleJMAP(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "user@example.test", "imap")

	orig := probeDomain
	t.Cleanup(func() { probeDomain = orig })
	probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
		return pjmap.Probe{
			SessionURL: "https://forged.example", MailAccountID: "forged",
		}, nil
	}

	if err := a.SwitchProtocol(id, "jmap"); err == nil {
		t.Fatal("expected ineligible JMAP rejection")
	}
	acc, _ := a.store.GetAccount(a.ctx, id)
	if acc.Protocol != "imap" {
		t.Fatalf("protocol = %q, want imap", acc.Protocol)
	}
}

func TestAddPasswordAccountIgnoresForgedJMAPFields(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	orig := probeDomain
	t.Cleanup(func() { probeDomain = orig })
	probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
		return pjmap.Probe{}, nil
	}

	_, err := a.AddPasswordAccount(AddAccountRequest{
		Email: "u@example.test", IMAPHost: "imap.example", IMAPPort: 993,
		Password: "pw", Protocol: "jmap",
	})
	if err == nil {
		t.Fatal("expected rejection of ineligible JMAP")
	}
}

func plantQuiescentWorker(a *App, accountID int64) *accountWorker {
	done := make(chan struct{})
	close(done)
	_, cancel := context.WithCancel(context.Background())
	w := &accountWorker{cancel: cancel, done: done}
	a.workersMu.Lock()
	a.workers[accountID] = w
	a.workersMu.Unlock()
	return w
}

func workerFor(a *App, accountID int64) *accountWorker {
	a.workersMu.Lock()
	defer a.workersMu.Unlock()
	return a.workers[accountID]
}

func TestSwitchProtocolTransactionFailureRestores(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "user@example.test", "jmap")

	msgID, err := a.store.InsertMessageWithAttachments(a.ctx, &storage.Message{
		AccountID: id, FolderID: folderID, UID: 2, Subject: "att",
	}, []storage.IncomingAttachment{{
		Filename: "note.txt", ContentType: "text/plain",
		Content: strings.NewReader("hello"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	attPath := filepath.Join(a.store.AttachmentsDir(), filepath.Join(
		filepath.Base(a.store.AttachmentsDir()), // placeholder; resolve via walk
	))
	_ = attPath
	var found string
	_ = filepath.Walk(a.store.AttachmentsDir(), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, "note.txt") {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("attachment file missing before switch")
	}
	_ = msgID

	oldWorker := plantQuiescentWorker(a, id)

	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, nil
	}
	a.store.TestingSetDeleteTxHook(func(context.Context) error {
		return errors.New("inject fail")
	})

	if err := a.SwitchProtocol(id, "imap"); err == nil {
		t.Fatal("expected transaction failure")
	}
	a.store.TestingSetDeleteTxHook(nil)

	acc, _ := a.store.GetAccount(a.ctx, id)
	if acc.Protocol != "jmap" {
		t.Fatalf("protocol = %q, want jmap after rollback", acc.Protocol)
	}
	if _, err := a.store.GetFolder(a.ctx, folderID); err != nil {
		t.Fatalf("folder should remain: %v", err)
	}
	if _, err := os.Stat(found); err != nil {
		t.Fatalf("attachment should be restored: %v", err)
	}
	books, _ := a.store.ListAddressBooks(a.ctx)
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1", len(books))
	}
	restarted := workerFor(a, id)
	if restarted == nil {
		t.Fatal("expected old worker restarted after transaction failure")
	}
	if restarted == oldWorker {
		t.Fatal("expected a new worker instance after restart")
	}
}

func TestSwitchProtocolSuccessRemovesStaging(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "user@example.test", "jmap")
	_, err := a.store.InsertMessageWithAttachments(a.ctx, &storage.Message{
		AccountID: id, FolderID: folderID, UID: 3, Subject: "att",
	}, []storage.IncomingAttachment{{
		Filename: "note.txt", ContentType: "text/plain",
		Content: strings.NewReader("hello"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var found string
	_ = filepath.Walk(a.store.AttachmentsDir(), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, "note.txt") {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("attachment missing")
	}

	oldWorker := plantQuiescentWorker(a, id)

	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, nil
	}
	if err := a.SwitchProtocol(id, "imap"); err != nil {
		t.Fatalf("SwitchProtocol: %v", err)
	}
	if _, err := os.Stat(found); !os.IsNotExist(err) {
		t.Fatalf("attachment should be gone after success, err=%v", err)
	}
	acc, _ := a.store.GetAccount(a.ctx, id)
	if acc.Protocol != "imap" {
		t.Fatalf("protocol = %q", acc.Protocol)
	}
	replaced := workerFor(a, id)
	if replaced == nil {
		t.Fatal("expected replacement worker after successful switch")
	}
	if replaced == oldWorker {
		t.Fatal("expected worker replaced, not same instance")
	}
}

func TestIdleLoopJMAPWatchRetryAndAbsent(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "watch@example.test", "jmap")
	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	orig := startWatch
	t.Cleanup(func() { startWatch = orig })
	origWait := idleRetryWaitFor
	t.Cleanup(func() { idleRetryWaitFor = origWait })
	idleRetryWaitFor = func(error) time.Duration { return 5 * time.Millisecond }

	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return &pjmap.Client{}, nil
	}
	t.Cleanup(func() { a.jmapClientForTest = nil })

	t.Run("absent capability", func(t *testing.T) {
		var dials atomic.Int32
		startWatch = func(context.Context, *pjmap.Client, string, pjmap.WatchEvents) (*pjmap.Watch, error) {
			dials.Add(1)
			return nil, pjmap.ErrNoWebSocket
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		jmapProtocol{a}.watch(ctx, *acc)
		if dials.Load() != 1 {
			t.Fatalf("absent capability dials = %d, want 1 (no retry)", dials.Load())
		}
	})

	t.Run("dial failures retry until cancel", func(t *testing.T) {
		var dials atomic.Int32
		startWatch = func(context.Context, *pjmap.Client, string, pjmap.WatchEvents) (*pjmap.Watch, error) {
			dials.Add(1)
			return nil, errors.New("dial failed")
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			jmapProtocol{a}.watch(ctx, *acc)
		}()
		deadline := time.After(2 * time.Second)
		for dials.Load() < 2 {
			select {
			case <-deadline:
				t.Fatalf("expected retries, dials=%d", dials.Load())
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
		cancel()
		<-done
	})
}

func TestPasswordCreateAndSwitchProvision(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	var calls atomic.Int32
	a.provisionCardDAV = func(ctx context.Context, account storage.Account, password string) error {
		calls.Add(1)
		for _, b := range []struct{ name, path string }{
			{"A", "/books/a/"}, {"B", "/books/b/"},
		} {
			existing, _ := a.store.ListAddressBooks(ctx)
			skip := false
			for _, e := range existing {
				if e.URL == "https://dav.example" && e.CollectionPath == b.path {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if _, err := a.store.CreateAddressBook(ctx, &storage.AddressBook{
				AccountID: account.ID, Name: b.name, URL: "https://dav.example",
				CollectionPath: b.path, Username: account.Email,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}

	dto, err := a.AddPasswordAccount(AddAccountRequest{
		Email: "pw@example.test", IMAPHost: "imap.example", IMAPPort: 993,
		Password: "secret", Protocol: "jmap",
	})
	if err != nil {
		t.Fatalf("AddPasswordAccount: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provision calls = %d, want 1", calls.Load())
	}
	books, _ := a.store.ListAddressBooks(a.ctx)
	if len(books) != 2 {
		t.Fatalf("books = %d, want 2", len(books))
	}

	calls.Store(0)
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, nil
	}
	if err := a.SwitchProtocol(dto.ID, "imap"); err != nil {
		t.Fatal(err)
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}
	if err := a.SwitchProtocol(dto.ID, "jmap"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("switch provision calls = %d, want 1", calls.Load())
	}
	books, _ = a.store.ListAddressBooks(a.ctx)
	if len(books) != 2 {
		t.Fatalf("after switch books = %d, want 2 (skipped existing)", len(books))
	}
}

func TestOAuthCreateNeverProvisions(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	var calls atomic.Int32
	a.provisionCardDAV = func(context.Context, storage.Account, string) error {
		calls.Add(1)
		return nil
	}
	a.oauthAuthorize = func(context.Context, string, string, string, string, func(string)) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}, nil
	}
	origProbe := probeDomain
	t.Cleanup(func() { probeDomain = origProbe })
	probeDomain = func(context.Context, pjmap.ProbeOptions, string, string, string, bool, ...string) (pjmap.Probe, error) {
		return pjmap.Probe{Available: true, SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}

	pending, err := a.BeginOAuthAccount(AddAccountRequest{
		Email: "o@example.test", Provider: "google", ClientID: "c",
		IMAPHost: "imap.example", IMAPPort: 993,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.FinishAddAccount(pending.ID, "jmap"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("oauth provision calls = %d, want 0", calls.Load())
	}
}

func TestDiscoveryFailureStillCreatesAccount(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}
	orig := discoverCardDAV
	t.Cleanup(func() { discoverCardDAV = orig })
	discoverCardDAV = func(context.Context, *http.Client, string) (string, error) {
		return "", errors.New("discover failed")
	}

	dto, err := a.AddPasswordAccount(AddAccountRequest{
		Email: "fail@example.test", IMAPHost: "imap.example", IMAPPort: 993,
		Password: "secret", Protocol: "jmap",
	})
	if err != nil {
		t.Fatalf("account create should succeed: %v", err)
	}
	if dto.ID == 0 {
		t.Fatal("expected account id")
	}
}

var _ io.Reader = strings.NewReader("")
