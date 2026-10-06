package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

// countingIMAP counts itself as running sync work from Login until Logout or
// Close, whichever comes first.
type countingIMAP struct {
	fakeIMAP
	running *atomic.Int32
	once    sync.Once
	in      atomic.Bool
}

func (c *countingIMAP) Login() error {
	c.in.Store(true)
	c.running.Add(1)
	return nil
}

func (c *countingIMAP) done() {
	if c.in.Load() {
		c.once.Do(func() { c.running.Add(-1) })
	}
}

func (c *countingIMAP) Logout() error { c.done(); return nil }
func (c *countingIMAP) Close() error  { c.done(); return nil }

// refillJob is a background sync job that keeps writing old-protocol rows into
// the account's cache until it is cancelled or the folder is gone, the way an
// IMAP body campaign does.
func refillJob(a *App, accountID, folderID int64, uid *atomic.Uint32, running *atomic.Int32) syncsched.Job {
	return syncsched.Job{
		Priority: syncsched.PriorityBackgroundBody,
		Kind:     syncsched.JobFetchBodies,
		FolderID: folderID,
		Run: func(ctx context.Context) error {
			running.Add(1)
			defer running.Add(-1)
			for ctx.Err() == nil {
				if _, err := a.store.InsertMessage(context.Background(), &storage.Message{
					AccountID: accountID, FolderID: folderID, UID: uid.Add(1), Subject: "refill",
				}); err != nil {
					return err
				}
			}
			return ctx.Err()
		},
	}
}

func seedMessages(t *testing.T, a *App, accountID, folderID int64, n int, uid *atomic.Uint32) {
	t.Helper()
	for range n {
		if _, err := a.store.InsertMessage(a.ctx, &storage.Message{
			AccountID: accountID, FolderID: folderID, UID: uid.Add(1), Subject: "seed",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// accountMessageCount counts the account's messages under oldFolder and under
// every folder it has now.
func accountMessageCount(t *testing.T, a *App, accountID, oldFolder int64) int {
	t.Helper()
	ids := []int64{oldFolder}
	folders, err := a.store.ListFolders(a.ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		ids = append(ids, f.ID)
	}
	n, err := a.store.CountMessages(a.ctx, storage.MessageQuery{FolderIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A protocol switch deletes the old cache in batches. Before the hold, any
// caller of ensureAccountSync (limit raise, message open, Sync, scroll, push)
// started a fresh scheduler mid-switch whose jobs wrote old-protocol rows
// between the batches, so the delete loop chased them for minutes. With the
// hold, nothing of the account runs while the cache is deleted, the switch is
// quick, and the new worker starts afterwards.
func TestSwitchProtocolHoldsSyncWhileDeleting(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "switch@example.test", "imap")
	var uid atomic.Uint32
	uid.Store(1)
	seedMessages(t, a, id, folderID, 1500, &uid)

	var running atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		return &countingIMAP{running: &running}, nil
	}
	a.onDemandFetchForTest = func(ctx context.Context, account storage.Account, folder storage.Folder, _ []string) error {
		running.Add(1)
		defer running.Add(-1)
		for range 50 {
			if _, err := a.store.InsertMessage(context.Background(), &storage.Message{
				AccountID: account.ID, FolderID: folder.ID, UID: uid.Add(1), Subject: "on demand",
			}); err != nil {
				return err
			}
		}
		return nil
	}
	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return nil, errors.New("no jmap server in this test")
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{SessionURL: "https://jmap.example", MailAccountID: "A1"}, nil
	}

	var batchesWithWork, batches atomic.Int32
	a.store.TestingSetDeleteTxHook(func(context.Context) error {
		batches.Add(1)
		if running.Load() > 0 {
			batchesWithWork.Add(1)
		}
		return nil
	})
	t.Cleanup(func() { a.store.TestingSetDeleteTxHook(nil) })

	// An in-flight background campaign on the account.
	tryScheduler(a, id).Enqueue(refillJob(a, id, folderID, &uid, &running))
	deadline := time.Now().Add(2 * time.Second)
	for running.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if running.Load() == 0 {
		t.Fatal("background job never started")
	}

	// Everything the UI and the push paths do while the switch runs.
	stub := &storage.Message{AccountID: id, FolderID: folderID, RemoteID: "stub-1", Subject: "stub"}
	if _, err := a.store.UpsertMessageListMeta(a.ctx, stub); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var hammer sync.WaitGroup
	hammerLoop := func(fn func()) {
		hammer.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				fn()
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
	hammerLoop(func() {
		if s := tryScheduler(a, id); s != nil {
			s.Enqueue(refillJob(a, id, folderID, &uid, &running))
		}
	})
	hammerLoop(func() { _ = a.TriggerSync() })
	hammerLoop(func() { _ = a.fetchMessageBodyOnDemand(stub) })

	start := time.Now()
	switched := make(chan error, 1)
	go func() { switched <- a.SwitchProtocol(id, "jmap") }()
	var err error
	select {
	case err = <-switched:
	case <-time.After(30 * time.Second):
		close(stop)
		hammer.Wait()
		t.Fatalf("switch still running after 30s (%d delete batches, %d with sync work running)",
			batches.Load(), batchesWithWork.Load())
	}
	elapsed := time.Since(start)
	close(stop)
	hammer.Wait()
	if err != nil {
		t.Fatalf("SwitchProtocol: %v", err)
	}

	if n := batchesWithWork.Load(); n > 0 {
		t.Errorf("%d of %d delete batches ran while a sync job of the account was running", n, batches.Load())
	}
	if !raceEnabled && elapsed > 5*time.Second {
		t.Errorf("switch took %v, want well under 5s", elapsed)
	}
	acc, gerr := a.store.GetAccount(a.ctx, id)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if acc.Protocol != "jmap" {
		t.Fatalf("protocol = %q, want jmap", acc.Protocol)
	}
	if n := accountMessageCount(t, a, id, folderID); n != 0 {
		t.Errorf("%d old-protocol messages left after the switch", n)
	}
	if workerFor(a, id) == nil {
		t.Error("no worker running after the switch")
	}
	if err := trySync(a, id); err != nil {
		t.Errorf("sync still held after the switch: %v", err)
	}
}

// Every way out of SwitchProtocol must lift the hold, or the mailbox never
// syncs again until restart.
func TestSwitchProtocolErrorPathsLiftHold(t *testing.T) {
	cases := []struct {
		name  string
		setup func(a *App)
	}{
		{"failing auth target", func(a *App) {
			a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
				return targetAuth{}, errors.New("login refused")
			}
		}},
		{"failing cache delete", func(a *App) {
			a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
				return targetAuth{}, nil
			}
			a.store.TestingSetDeleteTxHook(func(context.Context) error { return errors.New("inject fail") })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newJMAPSwitchTestApp(t)
			id, _ := seedSwitchAccount(t, a, "err@example.test", "jmap")
			a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
				return nil, errors.New("no jmap server in this test")
			}
			c.setup(a)
			t.Cleanup(func() { a.store.TestingSetDeleteTxHook(nil) })
			if err := a.SwitchProtocol(id, "imap"); err == nil {
				t.Fatal("expected the switch to fail")
			}
			if err := trySync(a, id); err != nil {
				t.Fatalf("sync still held after a failed switch: %v", err)
			}
		})
	}
}

// While an account is held, UI paths get a quiet no-op: opening a message
// shows the stub, and Sync does not report a failure.
func TestHeldAccountUIPathsAreQuiet(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "quiet@example.test", "imap")
	var fetched atomic.Int32
	a.onDemandFetchForTest = func(context.Context, storage.Account, storage.Folder, []string) error {
		fetched.Add(1)
		return nil
	}
	var dialed atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		dialed.Add(1)
		return &fakeIMAP{}, nil
	}
	stub := &storage.Message{AccountID: id, FolderID: folderID, RemoteID: "stub-1", Subject: "stub"}
	if _, err := a.store.UpsertMessageListMeta(a.ctx, stub); err != nil {
		t.Fatal(err)
	}

	release := a.holdAccountSync(id)
	if err := a.fetchMessageBodyOnDemand(stub); err != nil {
		t.Errorf("on-demand body while held = %v, want nil (stub shown)", err)
	}
	if err := a.SyncAccountNow(id); err != nil {
		t.Errorf("SyncAccountNow while held = %v, want nil", err)
	}
	if a.accountSync(id) != nil {
		t.Error("a held account got a scheduler")
	}
	if fetched.Load() != 0 || dialed.Load() != 0 {
		t.Errorf("held account ran sync work: %d fetches, %d connections", fetched.Load(), dialed.Load())
	}
	release()
	if err := a.fetchMessageBodyOnDemand(stub); err != nil {
		t.Fatalf("on-demand body after release: %v", err)
	}
	if fetched.Load() != 1 {
		t.Errorf("fetches after release = %d, want 1", fetched.Load())
	}
}

// Removing a mailbox is a hard cancel of that account only: its worker,
// sessions and scheduler stop, its state goes, and nothing brings them back.
// The other account keeps running.
func TestDeleteAccountHardCancelsOnlyThatAccount(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	idA, folderA := seedSwitchAccount(t, a, "gone@example.test", "imap")
	idB, _ := seedSwitchAccount(t, a, "stays@example.test", "imap")
	accA, err := a.store.GetAccount(a.ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	accB, err := a.store.GetAccount(a.ctx, idB)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	clients := map[string]*closeRecordingIMAP{}
	a.newIMAPClient = func(cfg pimap.Config) (mailClient, error) {
		c := &closeRecordingIMAP{}
		mu.Lock()
		clients[cfg.Username] = c
		mu.Unlock()
		return c, nil
	}
	clientFor := func(user string) *closeRecordingIMAP {
		mu.Lock()
		defer mu.Unlock()
		return clients[user]
	}

	release := make(chan struct{})
	var sessions sync.WaitGroup
	opened := make(chan struct{}, 2)
	for _, acc := range []*storage.Account{accA, accB} {
		acc := *acc
		sessions.Go(func() {
			_ = a.withIMAPSession(a.ctx, acc, func(mailClient) error {
				opened <- struct{}{}
				<-release
				return nil
			})
		})
	}
	defer func() { close(release); sessions.Wait() }()
	for range 2 {
		select {
		case <-opened:
		case <-time.After(2 * time.Second):
			t.Fatal("sessions never opened")
		}
	}

	type parked struct{ running, cancelled, release chan struct{} }
	park := func(id int64) parked {
		p := parked{make(chan struct{}), make(chan struct{}), make(chan struct{})}
		tryScheduler(a, id).Enqueue(syncsched.Job{
			Priority: syncsched.PriorityBackgroundBody,
			Kind:     syncsched.JobFetchBodies,
			Run: func(ctx context.Context) error {
				close(p.running)
				select {
				case <-ctx.Done():
					close(p.cancelled)
					return ctx.Err()
				case <-p.release:
					return nil
				}
			},
		})
		select {
		case <-p.running:
		case <-time.After(2 * time.Second):
			t.Fatalf("account %d job never started", id)
		}
		return p
	}
	jobA := park(idA)
	jobB := park(idB)
	defer close(jobA.release)
	defer close(jobB.release)
	plantQuiescentWorker(a, idA)

	if err := a.DeleteAccount(idA); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	if c := clientFor(accA.Email); c == nil || !c.closed.Load() {
		t.Error("removed account's session was not closed")
	}
	select {
	case <-jobA.cancelled:
	case <-time.After(2 * time.Second):
		t.Error("removed account's job was not cancelled")
	}
	if a.accountSync(idA) != nil {
		t.Error("removed account's scheduler survived")
	}
	if workerFor(a, idA) != nil {
		t.Error("removed account's worker survived")
	}
	a.accountStatesMu.Lock()
	_, hasState := a.accountStates[idA]
	a.accountStatesMu.Unlock()
	if hasState {
		t.Error("removed account's state entry was kept")
	}

	if c := clientFor(accB.Email); c == nil || c.closed.Load() {
		t.Error("other account's session was closed")
	}
	select {
	case <-jobB.cancelled:
		t.Error("other account's job was cancelled")
	case <-time.After(100 * time.Millisecond):
	}
	if a.accountSync(idB) == nil {
		t.Error("other account's scheduler was removed")
	}

	// Nothing brings the removed account's runtime back.
	if err := trySync(a, idA); err == nil {
		t.Error("ensureAccountSync recreated a removed account's runtime")
	}
	stale := &storage.Message{AccountID: idA, FolderID: folderA, RemoteID: "x"}
	_ = a.fetchMessageBodyOnDemand(stale)
	if a.accountSync(idA) != nil {
		t.Error("a scheduler came back for the removed account")
	}
	a.accountStatesMu.Lock()
	_, hasState = a.accountStates[idA]
	a.accountStatesMu.Unlock()
	if hasState {
		t.Error("a state entry came back for the removed account")
	}
}

// tryScheduler is the account's scheduler, or nil while it is held.
func tryScheduler(a *App, accountID int64) *syncsched.Scheduler {
	s, err := a.ensureAccountScheduler(a.ctx, accountID)
	if err != nil {
		return nil
	}
	return s
}

// trySync reports whether ensureAccountSync would start or return the
// account's runtime.
func trySync(a *App, accountID int64) error {
	_, err := a.ensureAccountSync(a.ctx, accountID)
	return err
}

// TriggerSync lists the accounts once and syncs them one after another, so an
// account switched while an earlier one synced reaches its job with the old
// protocol. That job must not run the old protocol's sync over the new
// protocol's folders.
func TestManualSyncSkipsAccountLoadedBeforeSwitch(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "stale@example.test", "imap")
	stale, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SwitchAccountProtocol(a.ctx, id, "jmap", "https://jmap.example", "A1", nil); err != nil {
		t.Fatal(err)
	}
	var dialed atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		dialed.Add(1)
		return &fakeIMAP{}, nil
	}
	if err := a.syncAccount(*stale, true); err != nil {
		t.Fatalf("sync with a stale protocol = %v, want nil", err)
	}
	if n := dialed.Load(); n != 0 {
		t.Errorf("stale IMAP sync opened %d connections on a JMAP account", n)
	}
}
